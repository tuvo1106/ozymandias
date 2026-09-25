package config

import (
	"strings"
	"testing"
	"time"
)

// TestStorage_RetentionZeroIsRejected.
//
// `retention: 0` read as "off" — max_bytes, two fields below it, uses zero for
// exactly that — but Validate never looked at the field and db.Options mapped
// an unset Retention to the 15-day default. An operator who meant "keep
// everything" got a store that silently started deleting blocks after a
// fortnight, with nothing in the log to say so. Negative is the way to say
// "keep everything"; zero now says nothing at all, loudly.
func TestStorage_RetentionZeroIsRejected(t *testing.T) {
	s := DefaultOzyd().Storage
	s.Retention = 0
	err := s.Validate()
	if err == nil {
		t.Fatal("retention: 0 was accepted; it means 15 days downstream, not unlimited")
	}
	if !strings.Contains(err.Error(), "storage.retention") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// TestStorage_RetentionNegativeAndPositiveAreBothFine — the two forms that do
// have a meaning, and an omitted key, which keeps the default rather than
// becoming zero (Load fills a DefaultOzyd, so absent is not the zero value).
func TestStorage_RetentionMeaningfulValuesPass(t *testing.T) {
	for _, d := range []time.Duration{-1, -24 * time.Hour, time.Hour, 15 * 24 * time.Hour} {
		s := DefaultOzyd().Storage
		s.Retention = d
		if err := s.Validate(); err != nil {
			t.Errorf("retention %v was rejected: %v", d, err)
		}
	}
	if err := DefaultOzyd().Storage.Validate(); err != nil {
		t.Errorf("the shipped default does not validate: %v", err)
	}
}
