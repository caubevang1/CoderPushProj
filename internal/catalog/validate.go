package catalog

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxSKULen  = 64  // products.sku VARCHAR(64)
	maxNameLen = 255 // products.name VARCHAR(255)
)

// Validate checks that an item is usable. A withdraw only needs a SKU and
// a version; any other item must also carry a name, a price and a currency.
func Validate(it Item) error {
	if strings.TrimSpace(it.SKU) == "" {
		return errors.New("missing sku")
	}
	if utf8.RuneCountInString(it.SKU) > maxSKULen {
		return fmt.Errorf("sku longer than %d characters", maxSKULen)
	}
	if it.Version <= 0 {
		return fmt.Errorf("version must be positive, got %d", it.Version)
	}
	if it.Withdrawn {
		return nil
	}
	if strings.TrimSpace(it.Name) == "" {
		return errors.New("missing name")
	}
	if utf8.RuneCountInString(it.Name) > maxNameLen {
		return fmt.Errorf("name longer than %d characters", maxNameLen)
	}
	if it.Price < 0 {
		return fmt.Errorf("price must not be negative, got %d", it.Price)
	}
	if !isCurrencyCode(it.Currency) {
		return fmt.Errorf("currency must be 3 uppercase letters, got %q", it.Currency)
	}
	return nil
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
