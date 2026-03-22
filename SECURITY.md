# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Toko-Mo-Co, please report it responsibly:

1. **Do NOT open a public GitHub issue** for security vulnerabilities
2. Email **security@scrollypedia.com** with details
3. Include steps to reproduce if possible
4. We will acknowledge receipt within 48 hours

## Scope

The following are in scope:

- Authentication bypass in the API key middleware
- PII redaction failures (sensitive data leaking through)
- SQL injection in the SQLite store
- WebSocket security issues
- Unauthorized access to dashboard endpoints

## Security Design

- **API keys**: SHA-256 hashed before storage; raw keys shown once at creation
- **PII redaction**: Regex-based with post-match validators (Luhn, IBAN mod-97)
- **Database**: SQLite in WAL mode with parameterized queries throughout
- **Auth cache**: Constant-time comparison via `crypto/subtle`

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest  | Yes       |
