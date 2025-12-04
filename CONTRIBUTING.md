# Contributing to Sartor

Thank you for your interest in contributing to Sartor! This document provides guidelines and instructions for contributing.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Coding Standards](#coding-standards)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Documentation](#documentation)

## Code of Conduct

This project adheres to a Code of Conduct that all contributors are expected to follow. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before contributing.

## Getting Started

### Prerequisites

- Go 1.24 or later
- Docker 17.03+
- kubectl 1.11.3+
- Access to a Kubernetes cluster (or use Kind for local development)
- Make

### Setting Up Your Development Environment

1. **Fork and Clone**

```bash
git clone https://github.com/YOUR_USERNAME/sartor.git
cd sartor
git remote add upstream https://github.com/sartorproj/sartor.git
```

2. **Install Dependencies**

```bash
go mod download
make manifests generate
```

3. **Run Tests**

```bash
make test
```

## Development Workflow

### 1. Create a Branch

```bash
git checkout -b feature/your-feature-name
# or
git checkout -b fix/your-bug-fix
```

### 2. Make Changes

- Write clean, well-documented code
- Follow the coding standards (see below)
- Add tests for new functionality
- Update documentation as needed

### 3. Test Your Changes

```bash
# Run unit tests
make test

# Run linter
make lint

# Run integration tests (requires envtest)
make test-integration
```

### 4. Commit Your Changes

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add support for GitLab provider
fix: correct memory calculation in recommender
docs: update installation guide
test: add unit tests for ArgoCD client
refactor: simplify Git provider interface
```

### 5. Push and Create Pull Request

```bash
git push origin feature/your-feature-name
```

Then create a Pull Request on GitHub.

## Coding Standards

### Go Style Guide

- Follow [Effective Go](https://go.dev/doc/effective_go)
- Use `gofmt` and `goimports` for formatting
- Run `golangci-lint` before committing
- Maximum line length: 120 characters (with exceptions for generated code)

### Code Organization

```
sartor/
├── api/              # CRD definitions
├── cmd/              # Entry points (controller, server)
├── internal/         # Private packages
│   ├── controller/   # Kubernetes controllers
│   ├── gitops/       # Git operations
│   ├── metrics/      # Prometheus client
│   └── recommender/  # Resource recommendation logic
└── docs/             # Documentation
```

### Naming Conventions

- Use descriptive names
- Exported functions/types should have clear documentation
- Avoid abbreviations unless widely understood
- Follow Go naming conventions (PascalCase for exported, camelCase for private)

### Error Handling

- Always handle errors explicitly
- Use `fmt.Errorf` with `%w` for error wrapping
- Provide context in error messages
- Log errors at appropriate levels

Example:

```go
if err != nil {
    return fmt.Errorf("failed to create Prometheus client: %w", err)
}
```

## Testing

### Unit Tests

- Write tests for all new functions
- Aim for >80% code coverage
- Use table-driven tests where appropriate
- Mock external dependencies

Example:

```go
func TestCalculateRecommendation(t *testing.T) {
    tests := []struct {
        name     string
        input    ResourceSpec
        expected ResourceSpec
    }{
        {
            name: "normal case",
            input: ResourceSpec{CPU: "100m"},
            expected: ResourceSpec{CPU: "120m"},
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := CalculateRecommendation(tt.input)
            assert.Equal(t, tt.expected, result)
        })
    }
}
```

### Integration Tests

- Use `envtest` for controller tests
- Test CRD reconciliation logic
- Verify API interactions

### Running Tests

```bash
# All tests
make test

# Specific package
go test ./internal/recommender/...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Submitting Changes

### Pull Request Process

1. **Update Documentation**
   - Update README.md if needed
   - Add/update code comments
   - Update API documentation

2. **Ensure Tests Pass**
   - All unit tests pass
   - Integration tests pass (if applicable)
   - Linter passes

3. **Create Pull Request**
   - Use a descriptive title
   - Fill out the PR template
   - Link related issues
   - Request review from maintainers

### PR Template

```markdown
## Description
Brief description of changes

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Breaking change
- [ ] Documentation update

## Testing
- [ ] Unit tests added/updated
- [ ] Integration tests added/updated
- [ ] Manual testing performed

## Checklist
- [ ] Code follows style guidelines
- [ ] Self-review completed
- [ ] Comments added for complex code
- [ ] Documentation updated
- [ ] No new warnings generated
- [ ] Tests pass locally
```

### Review Process

- Maintainers will review your PR
- Address feedback promptly
- Keep PRs focused and reasonably sized
- Squash commits before merging (if requested)

## Documentation

### Code Documentation

- Document all exported functions/types
- Use Go doc comments
- Include examples for complex functions

```go
// CalculateRecommendation calculates resource recommendations
// based on historical usage patterns and intent profile.
//
// Example:
//   rec := CalculateRecommendation(usage, IntentBalanced)
func CalculateRecommendation(usage UsageData, intent Intent) ResourceSpec {
    // ...
}
```

### User Documentation

- Update README.md for user-facing changes
- Add examples in `docs/examples/`
- Update configuration reference if needed

## Areas for Contribution

We welcome contributions in these areas:

- **New Features**: See [VISION.md](VISION.md) for roadmap
- **Bug Fixes**: Check [Issues](https://github.com/sartorproj/sartor/issues)
- **Documentation**: Improve guides and examples
- **Tests**: Increase coverage
- **Performance**: Optimize resource usage
- **Security**: Report vulnerabilities

## Getting Help

- **Questions**: Open a [Discussion](https://github.com/sartorproj/sartor/discussions)
- **Bugs**: Open an [Issue](https://github.com/sartorproj/sartor/issues)
- **Security**: Email security@sartorproj.io

## Recognition

Contributors will be:
- Listed in CONTRIBUTORS.md
- Mentioned in release notes
- Credited in documentation

Thank you for contributing to Sartor! 🎉
