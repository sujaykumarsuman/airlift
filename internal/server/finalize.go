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

// finalize runs once a beam has every chunk: the hash chain, the bundle stage
// and the downloads. It ends the beam in READY or FAILED, independent of every
// other beam in the place. On READY it writes the verified output under data_dir
// (ADR 0016) and serves downloads from those files, freeing the in-memory copies;
// a persist failure keeps the beam READY, served from memory, saved_path unset.
func (srv *Server) finalize(s *session.Session, b *session.Beam) {
	m, chunks, ok := s.BeamChunks(b)
	if !ok {
		return
	}
	bid := b.BID()
	out := session.Outcome{Downloads: map[string]session.Download{}}
	res := verify.Chain(chunks, m)
	out.Verdicts.GzSHA = &session.Verdict{OK: res.GzSHA.OK, Expected: res.GzSHA.Expected, Actual: res.GzSHA.Actual}
	if res.OrigSHA.Expected != "" {
		out.Verdicts.OrigSHA = &session.Verdict{OK: res.OrigSHA.OK, Expected: res.OrigSHA.Expected, Actual: res.OrigSHA.Actual}
	}
	if res.Err != nil {
		out.Err = res.Err.Error()
		srv.opts.Logf("session %s beam %s FAILED: %s", s.ID, bid, out.Err)
		s.FinishBeam(b, out)
		return
	}
	data := res.Data
	name := safeName(m.Name)
	out.Downloads["raw"] = session.Download{Name: name, ContentType: "application/octet-stream", Src: session.MemBlob(data)}

	var files []bundle.File
	var zipBytes []byte
	if bundle.IsBundle(data) {
		bun, err := bundle.Parse(data)
		if err != nil {
			out.Verdicts.Bundle = &session.Verdict{Expected: "well-formed repobundle", Actual: err.Error()}
			out.Err = "bundle: " + err.Error()
			srv.opts.Logf("session %s beam %s FAILED: %s", s.ID, bid, out.Err)
			s.FinishBeam(b, out)
			return
		}
		bad := bun.Bad()
		out.Verdicts.Bundle = &session.Verdict{
			OK:       len(bad) == 0,
			Expected: fmt.Sprintf("%d files, each matching its sha256", len(bun.Files)),
			Actual:   describeBad(bun, bad),
		}
		if len(bad) > 0 {
			out.Err = fmt.Sprintf("bundle: %d of %d files failed verification", len(bad), len(bun.Files))
			srv.opts.Logf("session %s beam %s FAILED: %s (%s)", s.ID, bid, out.Err, strings.Join(bad, ", "))
			s.FinishBeam(b, out)
			return
		}
		files = bun.Files
		paths := make([]string, 0, min(len(files), maxSummaryPaths))
		for _, f := range files[:min(len(files), maxSummaryPaths)] {
			paths = append(paths, f.Path)
		}
		out.Bundle = &session.BundleSummary{Files: len(files), TotalBytes: bun.TotalBytes(), Paths: paths}
		switch len(files) {
		case 0:
		case 1:
			out.Downloads["file"] = session.Download{Name: path.Base(files[0].Path), ContentType: "application/octet-stream", Src: session.MemBlob(files[0].Data)}
		default:
			z, err := bundle.Zip(files)
			if err != nil {
				out.Err = "bundle: zip: " + err.Error()
				srv.opts.Logf("session %s beam %s FAILED: %s", s.ID, bid, out.Err)
				s.FinishBeam(b, out)
				return
			}
			zipBytes = z
			out.Downloads["zip"] = session.Download{Name: stem(name) + ".zip", ContentType: "application/zip", Src: session.MemBlob(z)}
		}
	}

	// One finish instant for the snapshot and meta.json.
	finished := s.Now()
	out.FinishedAt = finished
	if srv.opts.DataDir != "" {
		meta := beamMeta{
			SID:           s.ID,
			BID:           bid,
			SenderSession: b.Sender,
			Name:          m.Name,
			State:         session.StateReady,
			GzSize:        m.GzSize,
			OrigSize:      m.OrigSize,
			GzSHA256:      res.GzSHA.Actual,
			OrigSHA256:    res.OrigSHA.Actual,
			Verdicts:      out.Verdicts,
			Bundle:        out.Bundle,
			Downloads:     downloadsList(out.Downloads),
			StartedAt:     s.BeamStartedAt(b),
			FinishedAt:    finished,
		}
		if dir, disk, err := srv.persistBeam(s.ID, bid, name, data, files, zipBytes, meta); err != nil {
			srv.opts.Logf("session %s beam %s: persist failed, serving from memory: %v", s.ID, bid, err)
		} else if s.Closed() {
			// The session was deleted or swept while we were writing, so its
			// evict-hook cleanup may have run before our files landed. close()
			// sets the closed flag before that cleanup, so a closed session seen
			// here means our directory would be orphaned; reclaim it ourselves.
			srv.removeSessionDir(s.ID)
		} else if s.BeamRemoved(b) {
			// The beam was removed or auto-evicted mid-verification; its dir
			// cleanup may have run before our write, so reclaim it.
			srv.removeBeamDir(s.ID, bid)
		} else {
			for k, d := range disk {
				out.Downloads[k] = d // swap MemBlob → fileBlob; the in-memory copy is now unreachable
			}
			out.SavedPath = dir
		}
	}

	s.FinishBeam(b, out)
	srv.opts.Logf("session %s beam %s READY: %s, %d bytes, gz %s, orig %s%s", s.ID, bid, name, len(data),
		res.GzSHA.Actual[:12], res.OrigSHA.Actual[:12], describeBundle(out.Bundle))
	// Update the session-level receipt now that a beam is READY (ADR 0013).
	srv.writeSessionJSON(s)
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
