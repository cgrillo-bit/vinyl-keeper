package types

type VinylMode uint8

const (
	ModeMyAlbum VinylMode = iota
	ModeScanCandidate
	ModeRegisterChoice
	ModePressingChoice
)

type VinylViewOpts struct {
	SimilarityPercent float64
	ShowConfidence    bool
	SelectedReleaseID int64
}
