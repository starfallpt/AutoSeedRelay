package strategy

import (
	"fmt"
	"time"
)

// RetirePolicy defines when a completed seed should be retired (removed from qB).
type RetirePolicy struct {
	MinSeeders  int     // Retire if seeders >= this.
	MinRatio    float64 // Retire if ratio >= this.
	MinDays     int     // Retire if seed age >= this many days.
	DeleteFiles bool    // Whether to delete data files when retiring.
}

// NewRetirePolicy creates a RetirePolicy with defaults.
func NewRetirePolicy(minSeeders int, minRatio float64, minDays int, deleteFiles bool) *RetirePolicy {
	return &RetirePolicy{
		MinSeeders:  minSeeders,
		MinRatio:    minRatio,
		MinDays:     minDays,
		DeleteFiles: deleteFiles,
	}
}

// SeedRecord is the data the retire policy needs about a seed.
type SeedRecord struct {
	InfoHash   string
	Seeders    int
	Ratio      float64
	Uploaded   int64 // bytes
	Downloaded int64 // bytes
	AddedOn    time.Time
	Completed  bool // whether the download is done
	State      string
}

// RetireDecision holds the result of a retire check.
type RetireDecision struct {
	ShouldRetire bool
	Reason       string
}

// ShouldRetire evaluates all retire conditions. At least one condition must
// be met for the seed to be retired. The seed must also be completed.
func (p *RetirePolicy) ShouldRetire(rec *SeedRecord) RetireDecision {
	if !rec.Completed {
		return RetireDecision{ShouldRetire: false, Reason: "not completed"}
	}

	// Check seeders condition.
	if p.MinSeeders > 0 && rec.Seeders >= p.MinSeeders {
		return RetireDecision{
			ShouldRetire: true,
			Reason:       fmt.Sprintf("seeders >= %d (current: %d)", p.MinSeeders, rec.Seeders),
		}
	}

	// Check ratio condition.
	if p.MinRatio > 0 && rec.Ratio >= p.MinRatio {
		return RetireDecision{
			ShouldRetire: true,
			Reason:       fmt.Sprintf("ratio >= %.2f (current: %.2f)", p.MinRatio, rec.Ratio),
		}
	}

	// Check age condition.
	if p.MinDays > 0 {
		age := time.Since(rec.AddedOn)
		if age >= time.Duration(p.MinDays)*24*time.Hour {
			return RetireDecision{
				ShouldRetire: true,
				Reason:       fmt.Sprintf("age >= %d days (current: %.1f days)", p.MinDays, age.Hours()/24),
			}
		}
	}

	return RetireDecision{ShouldRetire: false, Reason: "no retire condition met"}
}

// RetireReason returns a human-readable reason string for the retire decision.
func (p *RetirePolicy) RetireReason(rec *SeedRecord) string {
	d := p.ShouldRetire(rec)
	return d.Reason
}
