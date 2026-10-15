---
paths: src/legacy/**/*.go
---

# Error-handling rules

- GO-001: Always wrap errors with context using fmt.Errorf("doing X: %w", err).
