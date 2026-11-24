package server

// Each explicitly present child attribute overrides the corresponding
// inherited attribute, including a valid startNumber=0. A child timeline
// replaces the entire parent timeline; timelines are never concatenated.
func mergeDASHTemplate(parent, child *dashTemplate) *dashTemplate {
	if parent == nil && child == nil {
		return nil
	}
	var result dashTemplate
	if parent != nil {
		result = *parent
	}
	if child == nil {
		return &result
	}
	if child.Init != nil {
		result.Init = child.Init
	}
	if child.Media != nil {
		result.Media = child.Media
	}
	if child.StartNumber != nil {
		result.StartNumber = child.StartNumber
	}
	if child.Timescale != nil {
		result.Timescale = child.Timescale
	}
	if child.Duration != nil {
		result.Duration = child.Duration
	}
	if child.PresentationTimeOffset != nil {
		result.PresentationTimeOffset = child.PresentationTimeOffset
	}
	if child.Timeline != nil {
		result.Timeline = child.Timeline
	}
	return &result
}
