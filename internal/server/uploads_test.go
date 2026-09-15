package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

// registerSender registers a direct sender (ADR 0023) with the session token
// and returns its client id and session-admin flag.
func (h *harness) registerSender(t *testing.T, c created) (string, bool) {
	t.Helper()
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, "", []byte(`{"role":"sender"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register sender: %s %s", resp.Status, body)
	}
	var out struct {
		ClientID     string   `json:"client_id"`
		SessionAdmin bool     `json:"session_admin"`
		Roles        []string `json:"roles"`
	}
	json.Unmarshal(body, &out)
	if len(out.Roles) != 1 || out.Roles[0] != "sender" {
		t.Fatalf("a sender's reply should carry the sender role: %s", body)
	}
	return out.ClientID, out.SessionAdmin
}

func framesBody(frames []string) []byte {
	b, _ := json.Marshal(map[string]any{"frames": frames})
	return b
}

func uploadBody(d *beam.Dump) []byte {
	return []byte(fmt.Sprintf(`{"name":%q,"bytes":%d,"chunks":%d,"sender_session":%d}`, d.Manifest.Name, d.Manifest.GzSize, d.Manifest.Total(), d.SenderSession))
}

type uploadReply struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	By     string `json:"by"`
}

func (h *harness) pollUpload(t *testing.T, c created, clientID, id string) (int, uploadReply) {
	t.Helper()
	resp, body := h.do(t, "GET", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, clientID, nil)
	var u uploadReply
	json.Unmarshal(body, &u)
	return resp.StatusCode, u
}

// TestDirectUploadApproval walks a direct upload end to end: a sender's frames
// are refused until a session admin approves its request, the approval admits
// only the requested beam, the beam verifies, and the spent approval admits
// nothing more.
func TestDirectUploadApproval(t *testing.T) {
	h := start(t, nil)
	c := h.create(t) // the creator is the session admin
	sender, admin := h.registerSender(t, c)
	if admin {
		t.Fatal("a token joiner of a default session is not an admin")
	}
	d, err := beam.Encode(bytes.Repeat([]byte("direct upload "), 400), "up.txt", 300, 0xA11F7, beam.ModeSequential, 0)
	if err != nil {
		t.Fatal(err)
	}
	framesURL := "/api/sessions/" + c.SID + "/frames"

	// No request yet: the sender's frames are refused.
	if resp, _ := h.do(t, "POST", framesURL, c.Token, sender, framesBody(d.Frames)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames before a request: %s, want 403", resp.Status)
	}
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, sender, uploadBody(d))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request upload: %s %s", resp.Status, body)
	}
	var req uploadReply
	json.Unmarshal(body, &req)
	if req.Status != session.UploadPending || req.ID == "" {
		t.Fatalf("request reply %+v", req)
	}
	// The admin sees it — who, what and how big, never an address.
	snap := h.snapshot(t, c)
	if len(snap.Uploads) != 1 {
		t.Fatalf("snapshot uploads %+v", snap.Uploads)
	}
	u := snap.Uploads[0]
	if u.ID != req.ID || u.ClientID != sender || u.Client == "" || u.Name != "up.txt" || u.Chunks != d.Manifest.Total() || u.Bytes != d.Manifest.GzSize {
		t.Fatalf("upload view %+v", u)
	}
	raw, _ := json.Marshal(snap.Uploads)
	if strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), "addr") {
		t.Fatalf("the upload view leaks an address: %s", raw)
	}
	for _, cl := range snap.Clients {
		if cl.ID == sender && (len(cl.Roles) != 1 || cl.Roles[0] != "sender") {
			t.Fatalf("sender roles %v", cl.Roles)
		}
	}
	// Still pending: frames refused; the sender cannot approve itself.
	if code, p := h.pollUpload(t, c, sender, req.ID); code != 200 || p.Status != session.UploadPending {
		t.Fatalf("poll: %d %+v", code, p)
	}
	if resp, _ := h.do(t, "POST", framesURL, c.Token, sender, framesBody(d.Frames)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames while pending: %s, want 403", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+req.ID, c.Token, sender, []byte(`{"decision":"approve"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-admin approving: %s, want 403", resp.Status)
	}
	// A third participant can neither see nor cancel the request.
	viewer := h.registerAs(t, c, "")
	if code, _ := h.pollUpload(t, c, viewer, req.ID); code != http.StatusNotFound {
		t.Fatalf("a bystander polling: %d, want 404", code)
	}
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/uploads/"+req.ID, c.Token, viewer, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a bystander cancelling: %s, want 409", resp.Status)
	}

	// The admin approves.
	if resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+req.ID, c.Token, c.ClientID, []byte(`{"decision":"approve"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approve: %s %s", resp.Status, body)
	}
	if _, p := h.pollUpload(t, c, sender, req.ID); p.Status != session.UploadApproved || p.By != c.Name {
		t.Fatalf("after approval: %+v (admin %q)", p, c.Name)
	}
	if len(h.snapshot(t, c).Uploads) != 0 {
		t.Fatal("a decided request should leave the pending list")
	}
	// The approval is for this beam only: another beam's frames are bad.
	other, _ := beam.Encode([]byte("smuggled"), "other.txt", 300, 0xBAD, beam.ModeSequential, 0)
	_, body = h.do(t, "POST", framesURL, c.Token, sender, framesBody(other.Frames))
	var ing struct{ Accepted, Dup, Bad int }
	json.Unmarshal(body, &ing)
	if ing.Accepted != 0 || ing.Bad != len(other.Frames) {
		t.Fatalf("another beam under the approval: %s", body)
	}
	// The approved beam goes through and verifies.
	resp, body = h.do(t, "POST", framesURL, c.Token, sender, framesBody(d.Frames))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), fmt.Sprintf("%08x", d.SenderSession)) {
		t.Fatalf("approved frames: %s %s", resp.Status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b := h.oneBeam(t, c)
		if b.State == session.StateReady {
			break
		}
		if b.State == session.StateFailed || time.Now().After(deadline) {
			t.Fatalf("beam did not verify: %+v", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Spent: the request reads done and further frames are refused.
	if _, p := h.pollUpload(t, c, sender, req.ID); p.Status != session.UploadDone {
		t.Fatalf("after the beam: %+v", p)
	}
	if resp, _ := h.do(t, "POST", framesURL, c.Token, sender, framesBody(d.Frames[:1])); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames after the approval was spent: %s, want 403", resp.Status)
	}
}

// TestDirectUploadDenyCancelExpire covers the other endings: a denial, the
// sender's own cancel, expiry by the sweep's clock, and a session-admin sender
// approved at once.
func TestDirectUploadDenyCancelExpire(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	d, _ := beam.Encode([]byte("deny me"), "deny.txt", 300, 0xD0, beam.ModeSequential, 0)
	request := func() string {
		t.Helper()
		resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, sender, uploadBody(d))
		var r uploadReply
		json.Unmarshal(body, &r)
		if resp.StatusCode != http.StatusOK || r.Status != session.UploadPending {
			t.Fatalf("request: %s %s", resp.Status, body)
		}
		return r.ID
	}

	id := request()
	if again := request(); again != id {
		t.Fatalf("a second request while one is open should refresh it, got a new id %s (was %s)", again, id)
	}
	h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"deny"}`))
	if _, p := h.pollUpload(t, c, sender, id); p.Status != session.UploadDenied {
		t.Fatalf("denied poll %+v", p)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"approve"}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("deciding twice: %s, want 409", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, sender, framesBody(d.Frames)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames after a denial: %s, want 403", resp.Status)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"maybe"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad decision: %s, want 400", resp.Status)
	}

	// A fresh request after a denial is a new one; its sender withdraws it.
	id2 := request()
	if id2 == id {
		t.Fatal("a request after a denial should be new")
	}
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/uploads/"+id2, c.Token, sender, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %s", resp.Status)
	}
	if _, p := h.pollUpload(t, c, sender, id2); p.Status != session.UploadCancelled {
		t.Fatalf("cancelled poll %+v", p)
	}

	// Unanswered, it expires on the sweep's clock.
	id3 := request()
	s, _ := h.store.Get(c.SID)
	h.store.Sweep(s.Now().Add(session.UploadPendingTTL + time.Second))
	if _, p := h.pollUpload(t, c, sender, id3); p.Status != session.UploadExpired {
		t.Fatalf("expired poll %+v", p)
	}
	// Bad bodies are refused.
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, sender, []byte(`{"name":"","chunks":0}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty request: %s, want 400", resp.Status)
	}

	// In a joiners-admin session the sender is an admin: approved at once.
	_, ca := h.createOpts(t, `{"joiners_admin":true}`)
	adminSender, isAdmin := h.registerSender(t, ca)
	if !isAdmin {
		t.Fatal("a joiner of a joiners-admin session should be an admin")
	}
	resp, body := h.do(t, "POST", "/api/sessions/"+ca.SID+"/uploads", ca.Token, adminSender, uploadBody(d))
	if !strings.Contains(string(body), `"approved"`) || resp.StatusCode != http.StatusOK {
		t.Fatalf("an admin sender's request: %s %s", resp.Status, body)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+ca.SID+"/frames", ca.Token, adminSender, framesBody(d.Frames)); resp.StatusCode != http.StatusOK {
		t.Fatalf("an admin sender's frames: %s", resp.Status)
	}
	// Let the beam finish writing under data_dir before the test's temp dir goes.
	deadline := time.Now().Add(5 * time.Second)
	for h.oneBeam(t, ca).State != session.StateReady {
		if time.Now().After(deadline) {
			t.Fatalf("the admin sender's beam did not verify: %+v", h.oneBeam(t, ca))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSenderStreamIsPresence: a sender holding its event stream open shows as
// connected with only the sender tag (no viewer, no relay count).
func TestSenderStreamIsPresence(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.ts.URL+"/api/sessions/"+c.SID+"/events?role=sender", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", sender)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	snap := h.snapshot(t, c)
	if snap.Relays != 0 {
		t.Fatalf("a sender stream is not a relay: %d", snap.Relays)
	}
	found := false
	for _, cl := range snap.Clients {
		if cl.ID == sender {
			found = true
			if !cl.Connected || len(cl.Roles) != 1 || cl.Roles[0] != "sender" {
				t.Fatalf("sender presence %+v", cl)
			}
		}
	}
	if !found {
		t.Fatal("the sender is missing from the participants")
	}
	// The run ends: its stream closes with no request open, so it leaves the
	// list at once instead of idling there.
	cancel()
	listed := func() bool {
		for _, cl := range h.snapshot(t, c).Clients {
			if cl.ID == sender {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(2 * time.Second)
	for listed() {
		if time.Now().After(deadline) {
			t.Fatal("a finished sender should leave the participants list")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A keyed call (the same run coming back) returns it.
	h.do(t, "GET", "/api/sessions/"+c.SID, c.Token, sender, nil)
	if !listed() {
		t.Fatal("a sender that speaks again should be listed again")
	}
}

// TestSenderWithOpenRequestStaysListed: a sender whose stream drops while its
// request is still pending stays visible, so the admin can still decide.
func TestSenderWithOpenRequestStaysListed(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	d, _ := beam.Encode([]byte("wait for me"), "w.txt", 300, 0x77, beam.ModeSequential, 0)
	h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, sender, uploadBody(d))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", h.ts.URL+"/api/sessions/"+c.SID+"/events?role=sender", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Airlift-Client", sender)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Read(make([]byte, 64))
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.snapshot(t, c).Relays == 0 && len(h.snapshot(t, c).Uploads) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the handler notice the disconnect
	for _, cl := range h.snapshot(t, c).Clients {
		if cl.ID == sender {
			return
		}
	}
	t.Fatal("a sender with a pending request should stay listed")
}

// uploadAs posts an upload request for d as client and returns the status code
// and reply.
func (h *harness) uploadAs(t *testing.T, c created, client string, body []byte) (int, uploadReply) {
	t.Helper()
	resp, data := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads", c.Token, client, body)
	var r uploadReply
	json.Unmarshal(data, &r)
	return resp.StatusCode, r
}

func (h *harness) decideUpload(t *testing.T, c created, id, decision string) int {
	t.Helper()
	resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/uploads/"+id, c.Token, c.ClientID, []byte(`{"decision":"`+decision+`"}`))
	return resp.StatusCode
}

// TestUploadApprovalIsExact: what an admin approves is exactly what arrives —
// a request cannot change its beam once approved, a changed pending request
// is a new request, and the approval builds only a new beam matching the
// declared manifest.
func TestUploadApprovalIsExact(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	tiny, _ := beam.Encode([]byte("tiny"), "tiny.txt", 300, 1111, beam.ModeSequential, 0)
	big, _ := beam.Encode(bytes.Repeat([]byte("big payload "), 2000), "payload.bin", 300, 2222, beam.ModeSequential, 0)

	_, first := h.uploadAs(t, c, sender, uploadBody(tiny))
	if _, again := h.uploadAs(t, c, sender, uploadBody(tiny)); again.ID != first.ID {
		t.Fatal("an identical repeat should return the same request")
	}
	// Changed while pending: a new request; the admin's click on the old id admits nothing.
	_, swapped := h.uploadAs(t, c, sender, uploadBody(big))
	if swapped.ID == first.ID || swapped.Status != session.UploadPending {
		t.Fatalf("a changed pending request should be new: %+v (was %s)", swapped, first.ID)
	}
	if code := h.decideUpload(t, c, first.ID, "approve"); code != http.StatusConflict {
		t.Fatalf("approving the superseded request: %d, want 409", code)
	}
	if u := h.snapshot(t, c).Uploads; len(u) != 1 || u[0].ID != swapped.ID || u[0].Name != "payload.bin" {
		t.Fatalf("the admin should see only the current request: %+v", u)
	}
	// Approved: it cannot be swapped for another beam.
	h.decideUpload(t, c, swapped.ID, "approve")
	if code, _ := h.uploadAs(t, c, sender, uploadBody(tiny)); code != http.StatusConflict {
		t.Fatalf("swapping an approved request: %d, want 409", code)
	}
	// A manifest that differs from the declaration is refused, though its u32 matches.
	lie, _ := beam.Encode([]byte("not what was declared"), "payload.bin", 300, 2222, beam.ModeSequential, 0)
	_, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, sender, framesBody(lie.Frames[:1]))
	var ing struct{ Accepted, Bad int }
	json.Unmarshal(body, &ing)
	if ing.Accepted != 0 || ing.Bad != 1 || len(h.snapshot(t, c).Beams) != 0 {
		t.Fatalf("an undeclared manifest built a beam: %s", body)
	}
	// Revoked by the admin: the frames stop.
	if code := h.decideUpload(t, c, swapped.ID, "deny"); code != http.StatusNoContent {
		t.Fatalf("revoking an approval: %d", code)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, sender, framesBody(big.Frames)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("frames after a revoke: %s, want 403", resp.Status)
	}
}

// TestUploadApprovalOnlyBuildsItsOwnBeam: an approval cannot reach a beam some
// other participant created with the same u32, a request cannot name a beam
// already in the session, and removing the approved beam spends the approval.
func TestUploadApprovalOnlyBuildsItsOwnBeam(t *testing.T) {
	h := start(t, nil)
	c := h.create(t)
	sender, _ := h.registerSender(t, c)
	d, _ := beam.Encode(bytes.Repeat([]byte("mine "), 800), "mine.txt", 300, 0x5E1F, beam.ModeSequential, 0)
	_, req := h.uploadAs(t, c, sender, uploadBody(d))
	h.decideUpload(t, c, req.ID, "approve")

	// A scanner builds a different beam under the same u32 first.
	scanner := h.registerAs(t, c, "")
	squat, _ := beam.Encode([]byte("squatting"), "squat.txt", 300, 0x5E1F, beam.ModeSequential, 0)
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, scanner, framesBody(squat.Frames[:1]))
	_, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, sender, framesBody(d.Frames))
	var ing struct{ Accepted, Bad int }
	json.Unmarshal(body, &ing)
	if ing.Accepted != 0 || ing.Bad != len(d.Frames) {
		t.Fatalf("an approval reached another participant's beam: %s", body)
	}

	// A new request naming a beam already in the session is refused.
	other, _ := h.registerSender(t, c)
	if code, _ := h.uploadAs(t, c, other, uploadBody(squat)); code != http.StatusConflict {
		t.Fatalf("a request for a beam already present: %d, want 409", code)
	}

	// Removing the approved beam mid-receive spends its approval.
	noise := make([]byte, 3000) // incompressible: ten chunks, so two frames leave it receiving
	x := uint32(21)
	for i := range noise {
		x = x*1664525 + 1013904223
		noise[i] = byte(x >> 24)
	}
	d2, _ := beam.Encode(noise, "second.bin", 300, 0xBEE, beam.ModeSequential, 0)
	_, req2 := h.uploadAs(t, c, other, uploadBody(d2))
	h.decideUpload(t, c, req2.ID, "approve")
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, other, framesBody(d2.Frames[:2]))
	if resp, _ := h.do(t, "DELETE", "/api/sessions/"+c.SID+"/beams/"+fmt.Sprintf("%08x", d2.SenderSession), c.Token, c.ClientID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove beam: %s", resp.Status)
	}
	if _, p := h.pollUpload(t, c, other, req2.ID); p.Status != session.UploadDone {
		t.Fatalf("a removed beam's approval: %+v, want done", p)
	}
	if resp, _ := h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, other, framesBody(d2.Frames)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pushing a removed beam again: %s, want 403", resp.Status)
	}
}

// TestUploadLimitsAndHygiene: requests need the sender role, one address holds
// at most three pending, a keyless resume cannot make someone a sender, an
// evicted sender's request goes, empty POSTs keep no approval alive, a beam
// failing on arrival spends its approval, and ended records are forgotten.
func TestUploadLimitsAndHygiene(t *testing.T) {
	var clock testClock
	clock.t = time.Now()
	h := start(t, func(o *Options) {
		st := session.NewStore(time.Hour, 32)
		st.SetNow(clock.now)
		st.SetLimits(10, 64)
		o.Store = st
		o.Now = clock.now
	})
	c := h.create(t)
	noise := make([]byte, 2000) // incompressible, so its gzip is well over the 64-byte cap
	x := uint32(11)
	for i := range noise {
		x = x*1664525 + 1013904223
		noise[i] = byte(x >> 24)
	}
	d, _ := beam.Encode(noise, "big.bin", 300, 0xCA9, beam.ModeSequential, 0)

	viewer := h.registerAs(t, c, "")
	if code, _ := h.uploadAs(t, c, viewer, uploadBody(d)); code != http.StatusForbidden {
		t.Fatalf("a viewer requesting an upload: %d, want 403", code)
	}
	// A keyless resume from the same address cannot turn that viewer into a sender.
	resp, body := h.do(t, "POST", "/api/sessions/"+c.SID+"/clients", c.Token, viewer, []byte(`{"role":"sender"}`))
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "sender") {
		t.Fatalf("keyless promotion: %s %s", resp.Status, body)
	}
	if code, _ := h.uploadAs(t, c, viewer, uploadBody(d)); code != http.StatusForbidden {
		t.Fatal("a keyless resume must not have made the viewer a sender")
	}

	// Three pending per address; the fourth waits.
	var senders []string
	for i := 0; i < 4; i++ {
		s, _ := h.registerSender(t, c)
		senders = append(senders, s)
		di, _ := beam.Encode([]byte(fmt.Sprintf("n%d", i)), fmt.Sprintf("n%d.txt", i), 300, uint32(0x100+i), beam.ModeSequential, 0)
		code, _ := h.uploadAs(t, c, s, uploadBody(di))
		if want := map[bool]int{true: http.StatusOK, false: http.StatusConflict}[i < 3]; code != want {
			t.Fatalf("request %d from one address: %d, want %d", i+1, code, want)
		}
	}
	// Evicting a sender takes its request with it.
	h.do(t, "DELETE", "/api/sessions/"+c.SID+"/clients/"+senders[0], c.Token, c.ClientID, nil)
	if u := h.snapshot(t, c).Uploads; len(u) != 2 {
		t.Fatalf("after evicting a sender: %d pending, want 2", len(u))
	}

	// An approval kept only by empty POSTs expires; a beam over the size cap
	// fails on arrival and spends its approval.
	s1 := senders[1]
	_, open := h.uploadAs(t, c, s1, uploadBody(beamFor(t, senders, 1)))
	h.decideUpload(t, c, open.ID, "approve")
	for i := 0; i < 3; i++ {
		clock.mu.Lock()
		clock.t = clock.t.Add(4 * time.Minute)
		clock.mu.Unlock()
		h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, s1, []byte(`{"frames":[]}`))
	}
	h.store.Sweep(clock.now())
	if _, p := h.pollUpload(t, c, s1, open.ID); p.Status != session.UploadExpired {
		t.Fatalf("an approval kept alive by empty POSTs: %+v, want expired", p)
	}
	s2 := senders[2]
	_, capped := h.uploadAs(t, c, s2, uploadBody(d))
	if code, _ := h.uploadAs(t, c, s2, uploadBody(d)); code != http.StatusOK || capped.Status != session.UploadPending {
		t.Fatal("setup: the capped request should be pending")
	}
	h.decideUpload(t, c, capped.ID, "approve")
	h.do(t, "POST", "/api/sessions/"+c.SID+"/frames", c.Token, s2, framesBody(d.Frames[:1]))
	if _, p := h.pollUpload(t, c, s2, capped.ID); p.Status != session.UploadDone {
		t.Fatalf("a beam failing on arrival: %+v, want its approval done", p)
	}
	// Ended records are forgotten after a while.
	clock.mu.Lock()
	clock.t = clock.t.Add(11 * time.Minute)
	clock.mu.Unlock()
	h.store.Sweep(clock.now())
	if code, _ := h.pollUpload(t, c, s2, capped.ID); code != http.StatusNotFound {
		t.Fatalf("an ended record after the keep window: %d, want 404", code)
	}
}

// beamFor is the tiny beam request i of TestUploadLimitsAndHygiene declared.
func beamFor(t *testing.T, _ []string, i int) *beam.Dump {
	t.Helper()
	d, _ := beam.Encode([]byte(fmt.Sprintf("n%d", i)), fmt.Sprintf("n%d.txt", i), 300, uint32(0x100+i), beam.ModeSequential, 0)
	return d
}
