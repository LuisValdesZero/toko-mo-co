# Contributing to Toko-Mo-Co

Thank you for your interest in contributing! Here's how to get started.

## Development Setup

```bash
# Clone the repo
git clone https://github.com/scrollypedia/toko-mo-co.git
cd toko-mo-co

# Install Go 1.24+
go version

# Build
go build -o tokomoco .

# Run tests
go test ./...

# Run with race detector
go test -race ./...
```

## Project Structure

| Directory     | Purpose                                      |
|---------------|----------------------------------------------|
| `auth/`       | API key authentication middleware             |
| `cache/`      | LRU response cache with SQLite persistence    |
| `config/`     | Configuration loading & runtime settings API  |
| `dashboard/`  | HTML/JS/CSS frontend + WebSocket hub          |
| `detector/`   | Loop detection (Levenshtein similarity)       |
| `injector/`   | Warning injection into LLM responses          |
| `providers/`  | Custom provider management (Ollama, vLLM)     |
| `proxy/`      | Core reverse proxy handler                    |
| `redactor/`   | PII & secret redaction engine                 |
| `reliability/`| Retry with exponential backoff & fallback     |
| `rules/`      | Rules engine (model routing, rate limits)      |
| `store/`      | SQLite database layer                         |
| `tracker/`    | Session tracking, token counting, cost calc   |

## Making Changes

1. **Fork** the repository
2. **Create a branch**: `git checkout -b feature/my-feature`
3. **Write tests** for your changes
4. **Run the full suite**: `go test ./... && go vet ./...`
5. **Commit** with a clear message
6. **Open a PR** against `main`

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Table-driven tests preferred
- Keep functions focused and small
- Add comments for non-obvious logic

## What We're Looking For

- Bug fixes with test cases
- New PII detection patterns (with validators)
- Pricing updates for new LLM models
- Dashboard UI improvements
- Documentation improvements
- Performance optimizations

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
