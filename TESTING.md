# Testing Guide

This document describes the testing strategy and how to run tests for Sartor.

## Test Structure

```
sartor/
├── internal/
│   ├── recommender/
│   │   └── calculator_test.go      # Unit tests for recommender
│   ├── metrics/prometheus/
│   │   └── client_test.go          # Unit tests for Prometheus client
│   ├── gitops/
│   │   ├── writer/
│   │   │   ├── raw_test.go         # Raw YAML writer tests
│   │   │   ├── helm_test.go        # Helm writer tests
│   │   │   └── kustomize_test.go   # Kustomize writer tests
│   │   └── argocd/
│   │       └── client_test.go      # ArgoCD client tests
│   ├── notification/
│   │   └── webhook_test.go         # Webhook notification tests
│   ├── oom/
│   │   └── interceptor_test.go     # OOM interceptor tests
│   ├── server/api/
│   │   └── handlers_test.go        # API handler tests
│   └── controller/
│       ├── suite_test.go           # Test suite setup
│       ├── atelier_controller_test.go
│       └── tailoring_controller_test.go
```

## Running Tests

### All Tests

```bash
make test
```

### Unit Tests Only

```bash
make test-unit
```

### Integration Tests (requires envtest)

```bash
make test-integration
```

### Specific Package

```bash
go test ./internal/recommender/...
go test -v ./internal/metrics/prometheus/...
```

### With Coverage

```bash
make test-coverage
# Opens coverage.html in browser
```

### Watch Mode (for development)

```bash
# Install ginkgo watch
go install github.com/onsi/ginkgo/v2/ginkgo@latest

# Watch and run tests
ginkgo watch ./internal/recommender/...
```

## Test Types

### Unit Tests

- Test individual functions and methods
- Mock external dependencies
- Fast execution
- No Kubernetes cluster required

Example:

```go
func TestCalculateRecommendation(t *testing.T) {
    calc := NewCalculator()
    result := calc.Calculate(usage, intent)
    assert.Equal(t, expected, result)
}
```

### Integration Tests

- Test controller reconciliation
- Use `envtest` for Kubernetes API
- Test CRD interactions
- Require envtest setup

Example:

```go
var _ = Describe("Tailoring Controller", func() {
    It("Should create Cut on reconciliation", func() {
        tailoring := &Tailoring{...}
        Expect(k8sClient.Create(ctx, tailoring)).Should(Succeed())
        // Verify Cut was created
    })
})
```

## Test Coverage

Current coverage targets:

- **Overall**: >80%
- **Core packages**: >90%
- **Controllers**: >70%

View coverage:

```bash
make test-coverage
```

## Writing Tests

### Best Practices

1. **Use table-driven tests** for multiple scenarios:

```go
tests := []struct {
    name     string
    input    string
    expected string
}{
    {"case1", "input1", "output1"},
    {"case2", "input2", "output2"},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        result := Function(tt.input)
        assert.Equal(t, tt.expected, result)
    })
}
```

2. **Test edge cases**:
   - Empty inputs
   - Nil values
   - Boundary conditions
   - Error cases

3. **Use descriptive test names**:
   - `TestFunction_WithValidInput_ReturnsSuccess`
   - `TestFunction_WithNilInput_ReturnsError`

4. **Mock external dependencies**:
   - Use interfaces for testability
   - Create mock implementations
   - Use testify/mock for complex mocks

### Test Utilities

- **testify/assert**: Assertions
- **testify/require**: Assertions that stop test on failure
- **httptest**: HTTP server mocking
- **envtest**: Kubernetes API testing

## Continuous Integration

Tests run automatically on:

- Every push to main
- Every pull request
- Nightly builds

See `.github/workflows/ci.yml` for CI configuration.

## Debugging Tests

### Verbose Output

```bash
go test -v ./...
```

### Run Specific Test

```bash
go test -v -run TestSpecificFunction ./...
```

### Debug with Delve

```bash
dlv test ./internal/recommender/
(dlv) break TestFunction
(dlv) continue
```

## Performance Tests

For performance-critical code:

```go
func BenchmarkCalculate(b *testing.B) {
    for i := 0; i < b.N; i++ {
        Calculate(input)
    }
}
```

Run benchmarks:

```bash
go test -bench=. -benchmem ./...
```

## Test Data

- Use fixtures in `testdata/` directory
- Keep test data minimal and focused
- Use YAML files for complex test cases

## Troubleshooting

### Tests Fail Locally But Pass in CI

- Check Go version matches CI
- Ensure all dependencies are installed
- Run `go mod tidy`

### envtest Issues

```bash
# Reinstall envtest
make setup-envtest

# Check Kubernetes version
echo $ENVTEST_K8S_VERSION
```

### Flaky Tests

- Add retries for network-dependent tests
- Use deterministic test data
- Avoid time-dependent logic without mocking

## Resources

- [Go Testing Package](https://pkg.go.dev/testing)
- [testify Documentation](https://github.com/stretchr/testify)
- [Ginkgo Documentation](https://onsi.github.io/ginkgo/)
- [envtest Guide](https://book.kubebuilder.io/reference/envtest.html)
