package server

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// fileBlob serves a download from a file under data_dir. It satisfies
// session.Blob structurally, so the session package keeps no filesystem layout.
type fileBlob struct{ path string }

func (f fileBlob) Open() (io.ReadSeekCloser, error) { return os.Open(f.path) }

// beamMeta is the per-beam receipt written as meta.json (ADR 0016). It mirrors
// the dashboard's BeamSnapshot so the on-disk and in-memory records agree. The
// file is meta.json, not beam.json: ADR 0010 forbids a beam.json artifact on the
// user's disk.
type beamMeta struct {
	SID           string                 `json:"sid"`
	BID           string                 `json:"bid"`
	SenderSession uint32                 `json:"sender_session"`
	Name          string                 `json:"name"`
	State         session.State          `json:"state"`
	GzSize        int64                  `json:"gz_size"`
	OrigSize      int64                  `json:"orig_size"`
	GzSHA256      string                 `json:"gz_sha256"`
	OrigSHA256    string                 `json:"orig_sha256"`
	Verdicts      session.Verdicts       `json:"verdicts"`
	Bundle        *session.BundleSummary `json:"bundle"`
	Downloads     []string               `json:"downloads"`
	StartedAt     time.Time              `json:"started_at"`
	FinishedAt    time.Time              `json:"finished_at"`
}

// persistBeam writes beam bid of place sid to <data_dir>/<sid>/<bid>:
// raw/<name> (always), the unpacked tree/ (a bundle), <stem>.zip (a multi-file
// bundle) and meta.json. Everything is staged in a sibling temp dir and renamed
// into place, so a reader — or an operator browsing data_dir — never sees a
// half-written beam. On any error it removes the temp dir and returns the error;
// the caller keeps the beam READY and serves from memory. It returns the beam
// directory and the disk-backed downloads to swap in for the in-memory copies.
func (srv *Server) persistBeam(sid, bid, name string, raw []byte, files []bundle.File, zipBytes []byte, meta beamMeta) (string, map[string]session.Download, error) {
	sessDir := filepath.Join(srv.opts.DataDir, sid)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp(sessDir, "."+bid+"-")
	if err != nil {
		return "", nil, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tmp)
		}
	}()

	final := filepath.Join(sessDir, bid)
	disk := map[string]session.Download{}

	rawDir := filepath.Join(tmp, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(rawDir, name), raw, 0o644); err != nil {
		return "", nil, err
	}
	disk["raw"] = session.Download{Name: name, ContentType: "application/octet-stream", Src: fileBlob{filepath.Join(final, "raw", name)}}

	if len(files) > 0 {
		// WriteTree re-applies SafePath and refuses any !OK entry, so the tree
		// cannot escape <bid>/tree even if a bad entry reached here.
		if err := bundle.WriteTree(filepath.Join(tmp, "tree"), files); err != nil {
			return "", nil, err
		}
		switch len(files) {
		case 1:
			safe, err := bundle.SafePath(files[0].Path)
			if err != nil {
				return "", nil, err
			}
			disk["file"] = session.Download{
				Name:        path.Base(files[0].Path),
				ContentType: "application/octet-stream",
				Src:         fileBlob{filepath.Join(final, "tree", filepath.FromSlash(safe))},
			}
		default:
			zipName := stem(name) + ".zip"
			if err := os.WriteFile(filepath.Join(tmp, zipName), zipBytes, 0o644); err != nil {
				return "", nil, err
			}
			disk["zip"] = session.Download{Name: zipName, ContentType: "application/zip", Src: fileBlob{filepath.Join(final, zipName)}}
		}
	}

	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), append(blob, '\n'), 0o644); err != nil {
		return "", nil, err
	}

	if err := os.Rename(tmp, final); err != nil {
		return "", nil, err
	}
	committed = true
	return final, disk, nil
}

// removeSessionDir best-effort removes a place's tree; the store's evict hook
// runs it on DELETE and sweep-expiry. sid is 16-hex minted by the store; the
// guard is defence in depth. No token is ever logged.
func (srv *Server) removeSessionDir(sid string) {
	if srv.opts.DataDir == "" {
		return
	}
	if sid == "" || strings.ContainsAny(sid, `/\.`) {
		srv.opts.Logf("refusing to remove suspicious session dir %q", sid)
		return
	}
	if err := os.RemoveAll(filepath.Join(srv.opts.DataDir, sid)); err != nil {
		srv.opts.Logf("session %s: data cleanup failed: %v", sid, err)
	}
}

// removeBeamDir best-effort removes one beam's tree under a place. sid is 16-hex
// and bid 8-hex, both minted by airlift; the guards are defence in depth.
func (srv *Server) removeBeamDir(sid, bid string) {
	if srv.opts.DataDir == "" {
		return
	}
	if sid == "" || bid == "" || strings.ContainsAny(sid, `/\.`) || strings.ContainsAny(bid, `/\.`) {
		srv.opts.Logf("refusing to remove suspicious beam dir %q/%q", sid, bid)
		return
	}
	if err := os.RemoveAll(filepath.Join(srv.opts.DataDir, sid, bid)); err != nil {
		srv.opts.Logf("session %s beam %s: data cleanup failed: %v", sid, bid, err)
	}
}

// downloadsList reports which download kinds a map holds, in the snapshot order.
func downloadsList(m map[string]session.Download) []string {
	out := []string{}
	for _, k := range downloadKinds {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

var downloadKinds = []string{"raw", "file", "zip"}
