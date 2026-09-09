package server

import (
	"fmt"
	"path"
	"strings"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/verify"
)

const maxSummaryPaths = 50

// finalize runs once a session has every chunk: the hash chain, the bundle
// stage and the in-memory downloads. It ends in READY or FAILED. (Per-beam
// on-disk persistence under data_dir lands in a later step.)
func (srv *Server) finalize(s *session.Session) {
	m, chunks, ok := s.Chunks()
	if !ok {
		return
	}
	out := session.Outcome{Downloads: map[string]session.Download{}}
	res := verify.Chain(chunks, m)
	out.Verdicts.GzSHA = &session.Verdict{OK: res.GzSHA.OK, Expected: res.GzSHA.Expected, Actual: res.GzSHA.Actual}
	if res.OrigSHA.Expected != "" {
		out.Verdicts.OrigSHA = &session.Verdict{OK: res.OrigSHA.OK, Expected: res.OrigSHA.Expected, Actual: res.OrigSHA.Actual}
	}
	if res.Err != nil {
		out.Err = res.Err.Error()
		srv.opts.Logf("session %s FAILED: %s", s.ID, out.Err)
		s.Finish(out)
		return
	}
	data := res.Data
	name := safeName(m.Name)
	out.Downloads["raw"] = session.Download{Name: name, ContentType: "application/octet-stream", Data: data}

	var files []bundle.File
	if bundle.IsBundle(data) {
		b, err := bundle.Parse(data)
		if err != nil {
			out.Verdicts.Bundle = &session.Verdict{Expected: "well-formed repobundle", Actual: err.Error()}
			out.Err = "bundle: " + err.Error()
			srv.opts.Logf("session %s FAILED: %s", s.ID, out.Err)
			s.Finish(out)
			return
		}
		bad := b.Bad()
		out.Verdicts.Bundle = &session.Verdict{
			OK:       len(bad) == 0,
			Expected: fmt.Sprintf("%d files, each matching its sha256", len(b.Files)),
			Actual:   describeBad(b, bad),
		}
		if len(bad) > 0 {
			out.Err = fmt.Sprintf("bundle: %d of %d files failed verification", len(bad), len(b.Files))
			srv.opts.Logf("session %s FAILED: %s (%s)", s.ID, out.Err, strings.Join(bad, ", "))
			s.Finish(out)
			return
		}
		files = b.Files
		paths := make([]string, 0, min(len(files), maxSummaryPaths))
		for _, f := range files[:min(len(files), maxSummaryPaths)] {
			paths = append(paths, f.Path)
		}
		out.Bundle = &session.BundleSummary{Files: len(files), TotalBytes: b.TotalBytes(), Paths: paths}
		switch len(files) {
		case 0:
		case 1:
			out.Downloads["file"] = session.Download{Name: path.Base(files[0].Path), ContentType: "application/octet-stream", Data: files[0].Data}
		default:
			z, err := bundle.Zip(files)
			if err != nil {
				out.Err = "bundle: zip: " + err.Error()
				s.Finish(out)
				return
			}
			out.Downloads["zip"] = session.Download{Name: stem(name) + ".zip", ContentType: "application/zip", Data: z}
		}
	}

	s.Finish(out)
	srv.opts.Logf("session %s READY: %s, %d bytes, gz %s, orig %s%s", s.ID, name, len(data),
		res.GzSHA.Actual[:12], res.OrigSHA.Actual[:12], describeBundle(out.Bundle))
}

func describeBad(b *bundle.Bundle, bad []string) string {
	if len(bad) == 0 {
		return fmt.Sprintf("%d files verified", len(b.Files))
	}
	shown := bad
	if len(shown) > 5 {
		shown = shown[:5]
	}
	return fmt.Sprintf("%d failed: %s", len(bad), strings.Join(shown, ", "))
}

func describeBundle(s *session.BundleSummary) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf(", bundle of %d files", s.Files)
}

// safeName reduces the manifest's name to a single safe path component.
func safeName(name string) string {
	safe, err := bundle.SafePath(name)
	if err != nil {
		return "airlift-download"
	}
	return path.Base(safe)
}

// stem drops the extension: "repo-bundle.txt" → "repo-bundle".
func stem(name string) string {
	s := strings.TrimSuffix(name, path.Ext(name))
	if s == "" {
		return name
	}
	return s
}
