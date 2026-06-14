package controller

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
