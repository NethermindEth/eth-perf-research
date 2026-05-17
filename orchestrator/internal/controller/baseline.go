package controller

// The cold-start per-tx gas table and the unknown-verb fallback used to be
// package-level constants here; they now live in config.RunConfig
// (Control.BaseGasPerVerb / Control.DefaultBaseGasPerVerb) and are read off the
// State's resolved cfg. config.Defaults() carries the historical table, so the
// cold-start behaviour is unchanged.
//
// baselineGasPerVerb returns the cold-start gas estimate for verb, defaulting
// to cfg.Control.DefaultBaseGasPerVerb for unknown verbs and clamping zero to
// the default (so a misconfigured table entry can't divide by zero downstream).
//
// Per-verb estimates are upper bounds on the verb's exec-tx gas limit. Some
// verbs (storagespam, gasburnertx) genuinely consume 1.5-2M+ gas per tx, so
// under-estimating crashes the dispatcher's hard 0.95 × block-gas assertion.
func (s *State) baselineGasPerVerb(verb string) uint64 {
	def := s.cfg.Control.DefaultBaseGasPerVerb
	g, ok := s.cfg.Control.BaseGasPerVerb[verb]
	if !ok || g == 0 {
		return def
	}
	return g
}
