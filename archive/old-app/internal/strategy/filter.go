// Package strategy implements filter and retire logic for the v3 engine.
//
// Filter: determines which RSS items should be processed (promotion match,
// keyword match, size bounds).
//
// Retire: determines when a completed seed should be removed from qB.
package strategy

import (
	"strings"
)

// Filter matches RSS items against configurable rules.
type Filter struct {
	Promotions []string // e.g. ["free", "2x_free"]
	Keywords   []string // case-insensitive title match
	MinSize    int64    // 0 = no min
	MaxSize    int64    // 0 = no max
}

// NewFilter creates a Filter with the given rules.
func NewFilter(promotions, keywords []string, minSize, maxSize int64) *Filter {
	return &Filter{
		Promotions: promotions,
		Keywords:   keywords,
		MinSize:    minSize,
		MaxSize:    maxSize,
	}
}

// Promotion constants.
const (
	PromoFree     = "free"
	Promo2xFree   = "2x_free"
	Promo2x       = "2x"
	Promo50       = "50%"
	Promo30       = "30%"
	PromoNeutral  = "neutral"
)

var promoAliases = map[string][]string{
	PromoFree:    {"free", "免费", "freeleech"},
	Promo2xFree:  {"2x_free", "2x free", "2xfree", "double free", "双倍免费"},
	Promo2x:      {"2x", "double", "双倍"},
	Promo50:      {"50%", "half", "半价", "50"},
	Promo30:      {"30%", "30"},
	PromoNeutral: {"neutral", "普通", "normal"},
}

// MatchPromotion checks whether the discount type matches any configured promotion.
func (f *Filter) MatchPromotion(discount string) bool {
	if len(f.Promotions) == 0 {
		return true // No promotion filter = match all.
	}
	low := strings.ToLower(strings.TrimSpace(discount))
	for _, wanted := range f.Promotions {
		aliases, ok := promoAliases[wanted]
		if !ok {
			continue
		}
		for _, alias := range aliases {
			if strings.Contains(low, alias) {
				return true
			}
		}
	}
	return false
}

// MatchKeywords checks whether the title contains any of the configured keywords.
func (f *Filter) MatchKeywords(title string) bool {
	if len(f.Keywords) == 0 {
		return true // No keyword filter = match all.
	}
	low := strings.ToLower(title)
	for _, kw := range f.Keywords {
		if kw != "" && strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// MatchSize checks whether the seed size falls within the configured bounds.
// Zero values mean no limit on that side.
func (f *Filter) MatchSize(sizeBytes int64) bool {
	if f.MinSize > 0 && sizeBytes < f.MinSize {
		return false
	}
	if f.MaxSize > 0 && sizeBytes > f.MaxSize {
		return false
	}
	return true
}

// MatchAll runs all filters. An item is matched only if it passes all
// active filters. discount may be empty if the source doesn't provide it.
func (f *Filter) MatchAll(discount, title string, sizeBytes int64) bool {
	if !f.MatchPromotion(discount) {
		return false
	}
	if !f.MatchKeywords(title) {
		return false
	}
	if !f.MatchSize(sizeBytes) {
		return false
	}
	return true
}
