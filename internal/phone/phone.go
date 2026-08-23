package phone

import (
	"fmt"
	"strings"

	"github.com/nyaruka/phonenumbers"
)

func Normalize(value, countryCode string) (string, error) {
	region := strings.ToUpper(strings.TrimSpace(countryCode))
	parsed, err := phonenumbers.Parse(strings.TrimSpace(value), region)
	if err != nil || !phonenumbers.IsValidNumber(parsed) {
		return "", fmt.Errorf("invalid phone number")
	}
	return phonenumbers.Format(parsed, phonenumbers.E164), nil
}
