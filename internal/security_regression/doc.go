// Package security_regression holds behavioral regression tests for the
// platform's security invariants — tenant isolation, the existence-oracle
// boundary, privilege escalation, and agent containment. The DB-backed tests
// build under the `integration` tag and run as the real non-superuser
// manyforge_app role (see `make sec-test`).
package security_regression
