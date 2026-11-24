# Runbooks

One file per failure mode, named `<kebab-symptom>.md`, following
[_template.md](_template.md): Symptom, Cause, Fix, Verify, in that order.

| Runbook                                                                  | Symptom                                                |
| ------------------------------------------------------------------------ | ------------------------------------------------------ |
| [ready-returns-503.md](ready-returns-503.md)                             | `/ready` answers 503; the cycle log has an error       |
| [rate-limited-by-registry.md](rate-limited-by-registry.md)               | Instances show `rate limited until …`                  |
| [no-registry-ref.md](no-registry-ref.md)                                 | An instance shows `no registry ref`                    |
| [compose-drift.md](compose-drift.md)                                     | An instance shows `compose drift: runs …`              |
| [update-rolled-back.md](update-rolled-back.md)                           | `update-status` is `rolled-back` or `failed`           |
| [instance-storage-pool-not-found.md](instance-storage-pool-not-found.md) | `incus start` fails: `Instance storage pool not found` |
