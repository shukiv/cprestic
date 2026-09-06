# Deployment

- The default deployment target is `root@182.54.236.144`, unless the user
  explicitly chooses another server. Use this target for requested deployments.
- This server hosts production accounts even though it is also used for testing.
  Check for active backup, restore, and upgrade work before changing the service.
- Verify changes locally first, retain the previous binaries for rollback, and
  check service health and the affected pages after deployment.
- Do not run destructive restore tests against production accounts, alter
  backup data, or restart WHM/cpsrvd as part of a plugin UI deployment.
- Read-only reviews and reports do not authorize deployment or other mutations.
