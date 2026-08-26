package service

// Test-only export (idiomatic Go export_test.go pattern) so the external
// service_test package can compute the SAME advisory lock key LockedJob
// derives internally for a given job name, without duplicating the FNV-64a
// algorithm. service_test cannot import this package's unexported
// advisoryLockKey directly and cannot live in package service itself for
// tests that also need internal/postgrestest: postgrestest imports postgres,
// which imports service (postgres/checkpoint_integrity_store.go et al., for
// service.BalanceCheckpoint) -- so a package-service internal test file
// importing postgrestest back would be an import cycle. See
// worker_expiration_test.go.
var AdvisoryLockKeyForTest = advisoryLockKey
