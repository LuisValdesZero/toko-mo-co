package redactor

import (
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// PatternCategory defines a single detection category with its compiled regex
// patterns, optional post-match validator, and display metadata.
type PatternCategory struct {
	Key         string           // config key: "email", "ssn", "credit_card"
	Label       string           // display label: "Email Address"
	Group       string           // "pii" or "secret"
	Patterns    []*regexp.Regexp // pre-compiled regexes (MustCompile at init)
	Validate    func(string) bool // optional post-match validator (Luhn, IBAN)
	Placeholder string           // realistic fake for placeholder mode
	Tag         string           // e.g. "[REDACTED_EMAIL]"
}

// AllCategories is the ordered list of every detection category.
var AllCategories []PatternCategory

// CategoryMap provides O(1) lookup by key.
var CategoryMap map[string]*PatternCategory

// DefaultAllKeys returns the comma-separated string of all category keys.
func DefaultAllKeys() string {
	keys := make([]string, len(AllCategories))
	for i, c := range AllCategories {
		keys[i] = c.Key
	}
	return strings.Join(keys, ",")
}

func init() {
	AllCategories = []PatternCategory{
		// ── PII patterns ────────────────────────────────────────────────
		{
			Key:   "email",
			Label: "Email Address",
			Group: "pii",
			Patterns: compile(
				`[a-zA-Z0-9._%+\-]{1,64}@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`,
			),
			Placeholder: "user@example.com",
			Tag:         "[REDACTED_EMAIL]",
		},
		{
			Key:   "phone",
			Label: "Phone Number",
			Group: "pii",
			Patterns: compile(
				// US with parens: (555) 123-4567, (555)123-4567
				`\(\d{3}\)[\s.\-]?\d{3}[\s.\-]?\d{4}`,
				// US with separators: 555-123-4567, 555.123.4567, 555 123 4567
				// Requires at least one separator to avoid matching plain 10-digit numbers in financial data
				`\d{3}[\s.\-]\d{3}[\s.\-]\d{4}`,
				// International: +1 555 123 4567, +44 20 7946 0958
				`\+\d{1,3}[\s\-]\d[\d\s\-]{6,14}\d`,
			),
			Validate:    validatePhone,
			Placeholder: "555-000-0000",
			Tag:         "[REDACTED_PHONE]",
		},
		{
			Key:   "ssn",
			Label: "Social Security Number",
			Group: "pii",
			Patterns: compile(
				// Standard format with dashes; area 001-899 (excl 666), group 01-99, serial 0001-9999
				`\b(?:0[0-9][1-9]|0[1-9][0-9]|[1-578][0-9]{2}|6[0-57-9][0-9]|66[0-57-9])-(?:0[1-9]|[1-9][0-9])-(?:000[1-9]|00[1-9][0-9]|0[1-9][0-9]{2}|[1-9][0-9]{3})\b`,
			),
			Placeholder: "000-00-0000",
			Tag:         "[REDACTED_SSN]",
		},
		{
			Key:   "credit_card",
			Label: "Credit Card Number",
			Group: "pii",
			Patterns: compile(
				// 13-19 digit card numbers with optional spaces/dashes between groups
				`\b(?:\d[ \-]?){12,18}\d\b`,
			),
			Validate:    validateLuhn,
			Placeholder: "4111-XXXX-XXXX-XXXX",
			Tag:         "[REDACTED_CC]",
		},
		{
			Key:   "iban",
			Label: "IBAN",
			Group: "pii",
			Patterns: compile(
				// IBAN: 2 letter country code + 2 check digits + up to 30 alphanumeric
				`\b[A-Z]{2}\d{2}[\s]?[\dA-Z]{4}[\s]?(?:[\dA-Z]{4}[\s]?){1,7}[\dA-Z]{1,4}\b`,
			),
			Validate:    validateIBAN,
			Placeholder: "XX00-XXXX-XXXX-XXXX-XXXX-XX",
			Tag:         "[REDACTED_IBAN]",
		},
		{
			Key:   "ip_address",
			Label: "IP Address",
			Group: "pii",
			Patterns: compile(
				// IPv4 with valid octet ranges
				`\b(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\b`,
				// IPv6 (simplified — full and compressed forms)
				`\b(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}\b`,
				`\b(?:[0-9a-fA-F]{1,4}:){1,7}:\b`,
				`\b::(?:[0-9a-fA-F]{1,4}:){0,5}[0-9a-fA-F]{1,4}\b`,
			),
			Placeholder: "0.0.0.0",
			Tag:         "[REDACTED_IP]",
		},
		{
			Key:   "date_of_birth",
			Label: "Date of Birth",
			Group: "pii",
			Patterns: compile(
				// Context-aware: "born on MM/DD/YYYY", "DOB: MM-DD-YYYY", "date of birth: YYYY-MM-DD"
				`(?i)(?:born|dob|date\s+of\s+birth)[:\s]+\d{1,2}[/\-\.]\d{1,2}[/\-\.]\d{2,4}`,
				`(?i)(?:born|dob|date\s+of\s+birth)[:\s]+\d{4}[/\-\.]\d{1,2}[/\-\.]\d{1,2}`,
			),
			Placeholder: "01/01/1970",
			Tag:         "[REDACTED_DOB]",
		},
		{
			Key:   "zip_code",
			Label: "US Zip Code",
			Group: "pii",
			Patterns: compile(
				// Context-aware: "zip 90210", "postal code 90210-1234"
				`(?i)(?:zip|zip\s+code|postal|postal\s+code)[:\s]+\d{5}(?:\-\d{4})?`,
			),
			Placeholder: "00000",
			Tag:         "[REDACTED_ZIP]",
		},
		{
			Key:   "person_name",
			Label: "Person Name",
			Group: "pii",
			Patterns: compile(
				// Context-aware: only match names when preceded by an identifying keyword.
				// "my name is John Smith", "patient: Jane Doe", "user: Bob O'Brien"
				`(?i)(?:my\s+name\s+is|name[:\s]+|patient[:\s]+|user[:\s]+|customer[:\s]+|client[:\s]+|employee[:\s]+|applicant[:\s]+|person[:\s]+|contact[:\s]+|recipient[:\s]+|sender[:\s]+|author[:\s]+|signed[:\s]+)\s*([A-Z][a-z]{1,20}(?:\s+(?:[A-Z]\.?\s+)?[A-Z][a-z]{1,20}){1,3})`,
				// "Full name: First Last", "Name: First Middle Last"
				`(?i)(?:full\s+name|legal\s+name|real\s+name)[:\s]+\s*([A-Z][a-z]{1,20}(?:\s+(?:[A-Z]\.?\s+)?[A-Z][a-z]{1,20}){1,3})`,
			),
			Placeholder: "Jane Doe",
			Tag:         "[REDACTED_NAME]",
		},
		{
			Key:   "passport",
			Label: "Passport Number",
			Group: "pii",
			Patterns: compile(
				// Context-aware: "passport: AB1234567", "passport number 123456789"
				`(?i)(?:passport|passport\s+(?:no|num|number))[:\s]+[A-Z0-9]{6,12}`,
			),
			Placeholder: "XX0000000",
			Tag:         "[REDACTED_PASSPORT]",
		},
		{
			Key:   "drivers_license",
			Label: "Driver's License",
			Group: "pii",
			Patterns: compile(
				// Context-aware: "driver's license: D12345678", "DL number: AB-123-456"
				`(?i)(?:driver'?s?\s+license|DL|driving\s+license|license\s+number)[:\s]+[A-Z0-9][A-Z0-9\- ]{4,18}[A-Z0-9]`,
			),
			Placeholder: "DL-XXXXXX",
			Tag:         "[REDACTED_DL]",
		},
		{
			Key:   "medical_id",
			Label: "Medical/Health ID",
			Group: "pii",
			Patterns: compile(
				// Context-aware: "MRN: 1234567", "patient ID: ABC123", "health ID 12345"
				`(?i)(?:MRN|medical\s+record|patient\s+(?:id|number)|health\s+(?:id|plan)|member\s+id|insurance\s+id|policy\s+number|NPI)[:\s]+[A-Z0-9\-]{4,20}`,
			),
			Placeholder: "MRN-XXXXXX",
			Tag:         "[REDACTED_MEDICAL_ID]",
		},

		// ── Secret patterns ─────────────────────────────────────────────
		{
			Key:   "openai_key",
			Label: "OpenAI API Key",
			Group: "secret",
			Patterns: compile(
				`sk-[a-zA-Z0-9_\-]{20,}`,
			),
			Placeholder: "sk-...REDACTED",
			Tag:         "[REDACTED_OPENAI_KEY]",
		},
		{
			Key:   "aws_key",
			Label: "AWS Access Key",
			Group: "secret",
			Patterns: compile(
				`\bAKIA[0-9A-Z]{16}\b`,
			),
			Placeholder: "AKIA...REDACTED",
			Tag:         "[REDACTED_AWS_KEY]",
		},
		{
			Key:   "github_token",
			Label: "GitHub Token",
			Group: "secret",
			Patterns: compile(
				// GitHub PATs: ghp_, gho_, ghu_, ghs_, ghr_
				`\bgh[pousr]_[a-zA-Z0-9]{36,}\b`,
			),
			Placeholder: "ghp_...REDACTED",
			Tag:         "[REDACTED_GITHUB_TOKEN]",
		},
		{
			Key:   "generic_api_key",
			Label: "Generic API Key",
			Group: "secret",
			Patterns: compile(
				// Anthropic
				`sk-ant-[a-zA-Z0-9_\-]{20,}`,
				// Stripe live/test
				`sk_(?:live|test)_[a-zA-Z0-9]{20,}`,
				// Google AI
				`AIza[a-zA-Z0-9_\-]{35}`,
				// Slack bot/user tokens
				`xox[bp]\-[a-zA-Z0-9\-]+`,
			),
			Placeholder: "sk-...REDACTED",
			Tag:         "[REDACTED_API_KEY]",
		},
		{
			Key:   "jwt",
			Label: "JWT Token",
			Group: "secret",
			Patterns: compile(
				// JWT: header.payload.signature (base64url parts)
				`eyJ[a-zA-Z0-9_\-]{20,}\.eyJ[a-zA-Z0-9_\-]{20,}\.[a-zA-Z0-9_\-]{20,}`,
			),
			Placeholder: "eyJ...REDACTED",
			Tag:         "[REDACTED_JWT]",
		},
		{
			Key:   "bearer_token",
			Label: "Bearer Token",
			Group: "secret",
			Patterns: compile(
				// Bearer token in headers/strings — token must be 40+ chars
				`(?i)Bearer\s+[a-zA-Z0-9_\-\.]{40,}`,
			),
			Placeholder: "Bearer ...REDACTED",
			Tag:         "[REDACTED_BEARER]",
		},
		{
			Key:   "private_key",
			Label: "Private Key (PEM/SSH)",
			Group: "secret",
			Patterns: compile(
				// PEM private key blocks
				`-----BEGIN\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+|ENCRYPTED\s+)?PRIVATE\s+KEY-----`,
			),
			Placeholder: "-----BEGIN PRIVATE KEY-----...REDACTED",
			Tag:         "[REDACTED_PRIVATE_KEY]",
		},
		{
			Key:   "connection_string",
			Label: "Connection String",
			Group: "secret",
			Patterns: compile(
				// DB connection URLs with embedded credentials
				`(?i)(?:postgres|postgresql|mysql|mongodb|mongodb\+srv|redis|rediss|amqp|amqps)://[^\s'"]{10,}`,
			),
			Placeholder: "db://...REDACTED",
			Tag:         "[REDACTED_CONN_STRING]",
		},
		{
			Key:   "env_password",
			Label: "Env Password/Secret",
			Group: "secret",
			Patterns: compile(
				// KEY=value patterns for passwords/secrets/tokens
				`(?i)(?:[A-Z_]*(?:PASSWORD|SECRET|PWD|TOKEN|PASS))[=:]\s*['"]?[^\s'"]{4,}['"]?`,
			),
			Placeholder: "***=REDACTED",
			Tag:         "[REDACTED_ENV_SECRET]",
		},
	}

	// Build lookup map
	CategoryMap = make(map[string]*PatternCategory, len(AllCategories))
	for i := range AllCategories {
		CategoryMap[AllCategories[i].Key] = &AllCategories[i]
	}
}

// compile is a helper that compiles multiple regex patterns at init time.
func compile(patterns ...string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		compiled[i] = regexp.MustCompile(p)
	}
	return compiled
}

// ── Phone validator ─────────────────────────────────────────────────────────

// validatePhone rejects false-positive phone matches. Real phone numbers
// contain at least one formatting character (dash, dot, space, paren, or plus).
// A pure digit-only match is almost certainly a financial figure, not a phone.
func validatePhone(s string) bool {
	for _, ch := range s {
		switch ch {
		case '-', '.', ' ', '(', ')', '+':
			return true
		}
	}
	return false
}

// ── Luhn validator ──────────────────────────────────────────────────────────

// validateLuhn checks if a credit card number passes the Luhn algorithm.
// Input may contain spaces and dashes which are stripped first.
func validateLuhn(s string) bool {
	// Strip spaces and dashes
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)

	n := len(digits)
	if n < 13 || n > 19 {
		return false
	}

	sum := 0
	alt := false
	for i := n - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// ── IBAN mod-97 validator ───────────────────────────────────────────────────

// validateIBAN checks if an IBAN passes the mod-97 checksum validation.
// Spaces are stripped. Returns true if the rearranged numeric value mod 97 == 1.
func validateIBAN(s string) bool {
	// Strip spaces
	clean := strings.ReplaceAll(s, " ", "")
	n := len(clean)
	if n < 15 || n > 34 {
		return false
	}

	// Move first 4 characters to end
	rearranged := clean[4:] + clean[:4]

	// Convert letters to numbers: A=10, B=11, ..., Z=35
	var numeric strings.Builder
	for _, ch := range rearranged {
		if ch >= '0' && ch <= '9' {
			numeric.WriteRune(ch)
		} else if ch >= 'A' && ch <= 'Z' {
			val := int(ch - 'A' + 10)
			numeric.WriteString(strconv.Itoa(val))
		} else {
			return false // invalid character
		}
	}

	// Compute mod 97 using math/big for large numbers
	bigNum := new(big.Int)
	bigNum.SetString(numeric.String(), 10)
	mod := new(big.Int).Mod(bigNum, big.NewInt(97))
	return mod.Int64() == 1
}

