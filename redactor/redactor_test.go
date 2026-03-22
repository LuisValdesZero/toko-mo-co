package redactor

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── Redact core tests ─────────────────────────────────────────────────────

func allEnabledConfig() Config {
	cats := make(map[string]bool)
	for _, c := range AllCategories {
		cats[c.Key] = true
	}
	return Config{Enabled: true, Mode: "redact", Categories: cats}
}

func categoryConfig(keys ...string) Config {
	cats := make(map[string]bool)
	for _, k := range keys {
		cats[k] = true
	}
	return Config{Enabled: true, Mode: "redact", Categories: cats}
}

func TestRedact_Disabled(t *testing.T) {
	cfg := Config{Enabled: false}
	r := Redact("my email is test@example.com", cfg)
	if r.Text != "my email is test@example.com" {
		t.Error("disabled redaction should not modify text")
	}
	if len(r.Detections) != 0 {
		t.Error("disabled redaction should have 0 detections")
	}
}

func TestRedact_Email(t *testing.T) {
	cfg := categoryConfig("email")
	tests := []struct {
		input string
		want  string
	}{
		{"contact me at user@example.com please", "contact me at [REDACTED_EMAIL] please"},
		{"a.b+tag@sub.domain.co.uk", "[REDACTED_EMAIL]"},
		{"USER@DOMAIN.COM is uppercase", "[REDACTED_EMAIL] is uppercase"},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		if r.Text != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.input, r.Text, tt.want)
		}
	}
}

func TestRedact_Phone(t *testing.T) {
	cfg := categoryConfig("phone")
	tests := []struct {
		input string
		want  string
	}{
		{"call (555) 123-4567", "call [REDACTED_PHONE]"},
		{"555-123-4567", "[REDACTED_PHONE]"},
		{"555.123.4567", "[REDACTED_PHONE]"},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		if r.Text != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.input, r.Text, tt.want)
		}
	}
}

func TestRedact_Phone_FalsePositives(t *testing.T) {
	cfg := categoryConfig("phone")
	// Financial data should NOT be flagged as phone numbers
	falsePositives := []string{
		`"total_assets": 67732893696`,
		`"total_assets": 36840000000`,
		`"average_volume": 1523456789`,
		`volume: 4528970012`,
		`market cap $234567890123`,
	}
	for _, input := range falsePositives {
		r := Redact(input, cfg)
		if strings.Contains(r.Text, "[REDACTED_PHONE]") {
			t.Errorf("false positive: %q was flagged as phone, got %q", input, r.Text)
		}
	}
}

func TestRedact_PhoneInternational(t *testing.T) {
	cfg := categoryConfig("phone")
	r := Redact("+44 20 7946 0958", cfg)
	if !strings.Contains(r.Text, "[REDACTED_PHONE]") {
		t.Errorf("international phone should be redacted, got %q", r.Text)
	}
	if len(r.Detections) == 0 {
		t.Error("expected at least 1 phone detection for intl number")
	}
}

func TestRedact_SSN(t *testing.T) {
	cfg := categoryConfig("ssn")
	tests := []struct {
		input string
		want  string
	}{
		{"my ssn is 123-45-6789", "my ssn is [REDACTED_SSN]"},
		{"078-05-1120 is valid", "[REDACTED_SSN] is valid"},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		if r.Text != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.input, r.Text, tt.want)
		}
	}
}

func TestRedact_CreditCard_Luhn(t *testing.T) {
	cfg := categoryConfig("credit_card")

	// Valid Luhn number (Visa test card)
	r := Redact("card: 4111 1111 1111 1111", cfg)
	if !strings.Contains(r.Text, "[REDACTED_CC]") {
		t.Errorf("valid CC should be redacted, got %q", r.Text)
	}

	// Invalid Luhn — should NOT be redacted
	r2 := Redact("card: 4111 1111 1111 1110", cfg)
	if strings.Contains(r2.Text, "[REDACTED_CC]") {
		t.Errorf("invalid Luhn should not be redacted, got %q", r2.Text)
	}
}

func TestRedact_IPv4(t *testing.T) {
	cfg := categoryConfig("ip_address")
	r := Redact("server at 192.168.1.100", cfg)
	if !strings.Contains(r.Text, "[REDACTED_IP]") {
		t.Errorf("IPv4 should be redacted, got %q", r.Text)
	}
}

func TestRedact_OpenAIKey(t *testing.T) {
	cfg := categoryConfig("openai_key")
	r := Redact("key is sk-abc123def456ghi789jkl012mno", cfg)
	if !strings.Contains(r.Text, "[REDACTED_OPENAI_KEY]") {
		t.Errorf("OpenAI key should be redacted, got %q", r.Text)
	}
}

func TestRedact_AWSKey(t *testing.T) {
	cfg := categoryConfig("aws_key")
	r := Redact("aws key: AKIAIOSFODNN7EXAMPLE", cfg)
	if !strings.Contains(r.Text, "[REDACTED_AWS_KEY]") {
		t.Errorf("AWS key should be redacted, got %q", r.Text)
	}
}

func TestRedact_GitHubToken(t *testing.T) {
	cfg := categoryConfig("github_token")
	r := Redact("token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", cfg)
	if !strings.Contains(r.Text, "[REDACTED_GITHUB_TOKEN]") {
		t.Errorf("GitHub token should be redacted, got %q", r.Text)
	}
}

func TestRedact_PrivateKey(t *testing.T) {
	cfg := categoryConfig("private_key")
	r := Redact("-----BEGIN RSA PRIVATE KEY-----\nMIIE...", cfg)
	if !strings.Contains(r.Text, "[REDACTED_PRIVATE_KEY]") {
		t.Errorf("private key should be redacted, got %q", r.Text)
	}
}

func TestRedact_ConnectionString(t *testing.T) {
	cfg := categoryConfig("connection_string")
	tests := []string{
		"postgres://user:pass@localhost:5432/db",
		"mysql://root:secret@127.0.0.1/mydb",
		"mongodb+srv://admin:pwd@cluster0.mongodb.net/prod",
		"redis://default:pass@cache.example.com:6379",
	}
	for _, input := range tests {
		r := Redact(input, cfg)
		if !strings.Contains(r.Text, "[REDACTED_CONN_STRING]") {
			t.Errorf("connection string should be redacted: %q → %q", input, r.Text)
		}
	}
}

func TestRedact_EnvPassword(t *testing.T) {
	cfg := categoryConfig("env_password")
	r := Redact("DATABASE_PASSWORD=mysecretpass123", cfg)
	if !strings.Contains(r.Text, "[REDACTED_ENV_SECRET]") {
		t.Errorf("env password should be redacted, got %q", r.Text)
	}
}

func TestRedact_MultipleCategories(t *testing.T) {
	cfg := categoryConfig("email", "phone")
	r := Redact("email: test@example.com phone: 555-123-4567", cfg)
	if !strings.Contains(r.Text, "[REDACTED_EMAIL]") {
		t.Error("email should be redacted")
	}
	if !strings.Contains(r.Text, "[REDACTED_PHONE]") {
		t.Error("phone should be redacted")
	}
	if len(r.Detections) != 2 {
		t.Errorf("expected 2 detections, got %d", len(r.Detections))
	}
}

func TestRedact_CategoryFiltering(t *testing.T) {
	// Only email enabled — phone should pass through
	cfg := categoryConfig("email")
	r := Redact("email: test@example.com phone: 555-123-4567", cfg)
	if !strings.Contains(r.Text, "[REDACTED_EMAIL]") {
		t.Error("email should be redacted")
	}
	if strings.Contains(r.Text, "[REDACTED_PHONE]") {
		t.Error("phone should NOT be redacted when category is disabled")
	}
}

// ── Redaction modes ───────────────────────────────────────────────────────

func TestRedact_HashMode(t *testing.T) {
	cfg := categoryConfig("email")
	cfg.Mode = "hash"
	r := Redact("test@example.com", cfg)
	if !strings.Contains(r.Text, "[SHA:") {
		t.Errorf("hash mode should produce [SHA:...], got %q", r.Text)
	}
}

func TestRedact_PlaceholderMode(t *testing.T) {
	cfg := categoryConfig("email")
	cfg.Mode = "placeholder"
	r := Redact("test@example.com", cfg)
	if r.Text != "user@example.com" {
		t.Errorf("placeholder mode should produce placeholder, got %q", r.Text)
	}
}

// ── Validators ────────────────────────────────────────────────────────────

func TestValidateLuhn(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"4111111111111111", true},   // Visa test
		{"5500000000000004", true},   // Mastercard test
		{"340000000000009", true},    // Amex test
		{"4111111111111110", false},  // bad check digit
		{"1234567890", false},        // too short
		{"4111-1111-1111-1111", true}, // with dashes
		{"4111 1111 1111 1111", true}, // with spaces
	}
	for _, tt := range tests {
		got := validateLuhn(tt.input)
		if got != tt.valid {
			t.Errorf("validateLuhn(%q): got %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestValidateIBAN(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"DE89370400440532013000", true},  // German IBAN
		{"GB29NWBK60161331926819", true},  // UK IBAN
		{"FR7630006000011234567890189", true}, // French IBAN
		{"DE89370400440532013001", false}, // bad checksum
		{"XXXX", false},                    // too short
		{"DE89 3704 0044 0532 0130 00", true}, // with spaces
	}
	for _, tt := range tests {
		got := validateIBAN(tt.input)
		if got != tt.valid {
			t.Errorf("validateIBAN(%q): got %v, want %v", tt.input, got, tt.valid)
		}
	}
}

// ── ParseCategories ───────────────────────────────────────────────────────

func TestParseCategories(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]bool
	}{
		{"email,phone,ssn", map[string]bool{"email": true, "phone": true, "ssn": true}},
		{"email, phone , ssn", map[string]bool{"email": true, "phone": true, "ssn": true}},
		{"", map[string]bool{}},
		{"email", map[string]bool{"email": true}},
		{",,,", map[string]bool{}},
	}
	for _, tt := range tests {
		got := ParseCategories(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("ParseCategories(%q): got %d keys, want %d", tt.input, len(got), len(tt.want))
			continue
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Errorf("ParseCategories(%q)[%q]: got %v, want %v", tt.input, k, got[k], v)
			}
		}
	}
}

func TestDefaultAllKeys(t *testing.T) {
	keys := DefaultAllKeys()
	if keys == "" {
		t.Error("DefaultAllKeys should not be empty")
	}
	if !strings.Contains(keys, "email") {
		t.Error("DefaultAllKeys should contain 'email'")
	}
	if !strings.Contains(keys, "openai_key") {
		t.Error("DefaultAllKeys should contain 'openai_key'")
	}
}

// ── RedactRequestBody tests ───────────────────────────────────────────────

func TestRedactRequestBody_OpenAI(t *testing.T) {
	cfg := categoryConfig("email", "phone")
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"My email is test@example.com and phone is 555-123-4567"}]}`)

	newBody, count, cats, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 2 {
		t.Errorf("detection count: got %d, want 2", count)
	}
	if cats["email"] != 1 {
		t.Errorf("email count: got %d, want 1", cats["email"])
	}
	if cats["phone"] != 1 {
		t.Errorf("phone count: got %d, want 1", cats["phone"])
	}

	var parsed map[string]interface{}
	json.Unmarshal(newBody, &parsed)
	msgs := parsed["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"].(string)
	if strings.Contains(content, "test@example.com") {
		t.Error("email should be redacted in output")
	}
	if !strings.Contains(content, "[REDACTED_EMAIL]") {
		t.Error("email should be replaced with tag")
	}
}

func TestRedactRequestBody_Anthropic(t *testing.T) {
	cfg := categoryConfig("email")
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"Email: test@example.com"}]}]}`)

	newBody, count, _, err := RedactRequestBody(body, "anthropic", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 1 {
		t.Errorf("detection count: got %d, want 1", count)
	}

	var parsed map[string]interface{}
	json.Unmarshal(newBody, &parsed)
	msgs := parsed["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if strings.Contains(text, "test@example.com") {
		t.Error("email should be redacted in Anthropic content block")
	}
}

func TestRedactRequestBody_SystemField(t *testing.T) {
	cfg := categoryConfig("email")
	body := []byte(`{"model":"claude-sonnet-4","system":"Contact support@example.com","messages":[{"role":"user","content":"Hello"}]}`)

	_, count, _, err := RedactRequestBody(body, "anthropic", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 1 {
		t.Errorf("detection count: got %d, want 1 (system field)", count)
	}
}

func TestRedactRequestBody_Disabled(t *testing.T) {
	cfg := Config{Enabled: false}
	body := []byte(`{"messages":[{"role":"user","content":"test@example.com"}]}`)

	newBody, count, _, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("disabled should have 0 detections, got %d", count)
	}
	if string(newBody) != string(body) {
		t.Error("disabled should not modify body")
	}
}

func TestRedactRequestBody_NoPII(t *testing.T) {
	cfg := categoryConfig("email")
	body := []byte(`{"messages":[{"role":"user","content":"Hello, how are you?"}]}`)

	newBody, count, _, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 detections, got %d", count)
	}
	// Body should be returned unchanged (original bytes)
	if string(newBody) != string(body) {
		t.Error("body with no PII should be returned unchanged")
	}
}

func TestRedactRequestBody_InvalidJSON(t *testing.T) {
	cfg := categoryConfig("email")
	body := []byte("not json")

	_, _, _, err := RedactRequestBody(body, "openai", cfg)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// ── L2: New PII pattern tests ─────────────────────────────────────────────

func TestRedact_PersonName(t *testing.T) {
	cfg := categoryConfig("person_name")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"my name is John Smith", true},
		{"patient: Jane Doe", true},
		{"user: Bob Johnson", true},
		{"customer: Alice Williams", true},
		{"full name: Robert Brown", true},
		{"legal name: Maria Garcia", true},
		// Should NOT match without context keyword
		{"John Smith went to the store", false},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_NAME]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

func TestRedact_Passport(t *testing.T) {
	cfg := categoryConfig("passport")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"passport: AB1234567", true},
		{"passport number 123456789", true},
		{"passport no C9876543", true},
		// Should NOT match without context keyword
		{"AB1234567 is a code", false},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_PASSPORT]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

func TestRedact_DriversLicense(t *testing.T) {
	cfg := categoryConfig("drivers_license")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"driver's license: D12345678", true},
		{"DL: AB-123-456-789", true},
		{"driving license: X9876543", true},
		{"license number: S123-4567-8901", true},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_DL]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

func TestRedact_MedicalID(t *testing.T) {
	cfg := categoryConfig("medical_id")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"MRN: 1234567", true},
		{"patient ID: ABC12345", true},
		{"health plan: HP-98765", true},
		{"member id: MEM-001234", true},
		{"insurance id: INS-5678", true},
		{"policy number: POL-9999", true},
		{"NPI: 1234567890", true},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_MEDICAL_ID]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

func TestRedact_DateOfBirth(t *testing.T) {
	cfg := categoryConfig("date_of_birth")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"DOB: 01/15/1990", true},
		{"born 1985-03-22", true},
		{"date of birth: 12/25/2000", true},
		// Should NOT match a standalone date without context
		{"12/25/2000 is a date", false},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_DOB]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

func TestRedact_ZipCode(t *testing.T) {
	cfg := categoryConfig("zip_code")
	tests := []struct {
		input    string
		redacted bool
	}{
		{"zip 90210", true},
		{"zip code: 90210-1234", true},
		{"postal code 10001", true},
		// Should NOT match a standalone number without context
		{"90210 is a number", false},
	}
	for _, tt := range tests {
		r := Redact(tt.input, cfg)
		hasTag := strings.Contains(r.Text, "[REDACTED_ZIP]")
		if hasTag != tt.redacted {
			t.Errorf("Redact(%q): hasTag=%v, want %v (got %q)", tt.input, hasTag, tt.redacted, r.Text)
		}
	}
}

// ── L3: Name field scanning tests ─────────────────────────────────────────

func TestRedactRequestBody_NameField_OpenAI(t *testing.T) {
	cfg := categoryConfig("email")
	// PII in the "name" field should be caught (field-splitting bypass)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","name":"test@example.com","content":"Hello there"}]}`)

	newBody, count, cats, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 1 {
		t.Errorf("detection count: got %d, want 1", count)
	}
	if cats["email"] != 1 {
		t.Errorf("email count: got %d, want 1", cats["email"])
	}

	var parsed map[string]interface{}
	json.Unmarshal(newBody, &parsed)
	msgs := parsed["messages"].([]interface{})
	name := msgs[0].(map[string]interface{})["name"].(string)
	if strings.Contains(name, "test@example.com") {
		t.Error("email in name field should be redacted")
	}
	if !strings.Contains(name, "[REDACTED_EMAIL]") {
		t.Error("email in name field should be replaced with tag")
	}
}

func TestRedactRequestBody_NameField_BothFields(t *testing.T) {
	cfg := categoryConfig("email", "phone")
	// PII split across name and content fields — both should be caught
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","name":"test@example.com","content":"call me at 555-123-4567"}]}`)

	_, count, cats, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 2 {
		t.Errorf("detection count: got %d, want 2 (1 name + 1 content)", count)
	}
	if cats["email"] != 1 {
		t.Errorf("email count: got %d, want 1", cats["email"])
	}
	if cats["phone"] != 1 {
		t.Errorf("phone count: got %d, want 1", cats["phone"])
	}
}

func TestRedactRequestBody_NameField_NoName(t *testing.T) {
	cfg := categoryConfig("email")
	// Messages without a name field should still work fine
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"test@example.com"}]}`)

	_, count, _, err := RedactRequestBody(body, "openai", cfg)
	if err != nil {
		t.Fatalf("RedactRequestBody: %v", err)
	}
	if count != 1 {
		t.Errorf("detection count: got %d, want 1", count)
	}
}

func TestDefaultAllKeys_IncludesNewCategories(t *testing.T) {
	keys := DefaultAllKeys()
	for _, key := range []string{"person_name", "passport", "drivers_license", "medical_id"} {
		if !strings.Contains(keys, key) {
			t.Errorf("DefaultAllKeys should contain %q, got %q", key, keys)
		}
	}
}
