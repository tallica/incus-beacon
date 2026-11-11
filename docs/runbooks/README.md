# Runbooks

One file per failure mode, named `<kebab-symptom>.md`, following
[_template.md](_template.md): Symptom, Cause, Fix, Verify, in that order.

| Runbook                                                    | Symptom                                          |
| ---------------------------------------------------------- | ------------------------------------------------ |
| [ready-returns-503.md](ready-returns-503.md)               | `/ready` answers 503; the cycle log has an error |
| [rate-limited-by-registry.md](rate-limited-by-registry.md) | Instances show `rate limited until …`            |
| [no-registry-ref.md](no-registry-ref.md)                   | An instance shows `no registry ref`              |
| [compose-drift.md](compose-drift.md)                       | An instance shows `compose drift: runs …`        |
