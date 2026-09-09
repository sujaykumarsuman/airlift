package beam

// Options configure a beam build. Zero values are filled with the defaults
// `airlift beam` uses.
type Options struct {
	Chunk         int
	ECC           string // "L", "M", "Q" or "H"
	FPS           int
	ManifestEvery int
	Seed          *int64 // nil for a random sender session
	Mode          Mode   // ModeAuto unless overridden
}

// Result is a built beam: the self-contained HTML page and the facts a caller
// prints in its summary.
type Result struct {
	HTML        string
	Dump        *Dump
	Version     int
	SizeModules int
	Order       []int
	Fountain    bool
	Packets     int // fountain packets, 0 in sequential layout
}

// Build runs the whole beam pipeline for one payload: encode (mode auto by
// default), render every frame at one QR version, lay out the loop, and fill
// the player template. It is what `airlift beam` calls once it has the payload
// bytes and the name.
func Build(data []byte, name string, o Options) (*Result, error) {
	if o.Chunk == 0 {
		o.Chunk = DefaultChunk
	}
	if o.ECC == "" {
		o.ECC = "M"
	}
	if o.FPS == 0 {
		o.FPS = 8
	}
	if o.ManifestEvery == 0 {
		o.ManifestEvery = 20
	}
	session := NewSession(o.Seed)
	d, err := Encode(data, name, o.Chunk, session, o.Mode, 0)
	if err != nil {
		return nil, err
	}
	version, size, paths, err := RenderQR(d.Frames, o.ECC)
	if err != nil {
		return nil, err
	}
	order := Schedule(len(d.Frames)-1, o.ManifestEvery)
	res := &Result{
		Dump:        d,
		Version:     version,
		SizeModules: size,
		Order:       order,
		Fountain:    d.Fountain != nil,
	}
	if d.Fountain != nil {
		res.Packets = d.Fountain.Packets
	}
	res.HTML = PlayerHTML(name, session, len(d.Frames)-1, order, paths, size, o.FPS, res.Fountain)
	return res, nil
}
