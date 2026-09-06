# Agent Notes

- Local integration runs can use the repository `.env`; load it with `set -a; source .env; set +a` before running commands that need Modal, Turbopuffer, AWS, or other service credentials.
- Do not print `.env` values or include secrets in logs. If checking available variables, redact values.

## Generality

- Do not add instance-specific behavior anywhere in the codebase: no special
  cases for a developer's filesystem, user, root, directory, filename, document,
  fixture identity, observed sample output, or particular test run.
- Product behavior must follow general file-format/content contracts and
  explicit configuration, never recognition of our data or test scenarios.
- Keep synthetic inputs and expected outputs in test fixture data. Shared test
  validators must check those supplied expectations, not branch on filenames
  or excuse a particular observed result.
- Exercise failures through external process/network boundaries. Do not add
  production test modes, fault hooks, bypasses, or alternate processing paths.

## Testing

- Only end-to-end tests. Do not add or maintain unit tests, mock-based tests,
  or in-process integration tests.
- Run the production CLI, API, consumers and worker roles as separate processes
  in Docker Compose, using production migrations and real network interfaces.
- Exercise user-visible workflows through the CLI/API. Database, queue and
  object-store inspection is allowed for assertions, not to bypass the workflow.
- Use real Postgres and S3/SQS-compatible services. Keep external model/search
  providers real; missing credentials must fail the run, not skip tests or select
  a fake. Document every remaining difference from production.
- Cover capture through search/read, updates, deletes, retries, restarts,
  authorization and durable side effects. Never report an unrun scenario as
  verified. Use synthetic fixtures and isolated resources only.

## Architecture communication

When explaining system architecture:

- Lead with separately deployable roles: API server, dispatcher,
  transformation workers, collector, index workers, local agent, etc.
- Distinguish deployment roles from hosting platforms. “Modal,” “AWS,”
  or “Kubernetes” describes where something runs, not what the system is.
- Distinguish one codebase, one deployment, and multiple runtime workers.
- Show a concrete diagram with named components and labeled arrows.
  Include queues, databases, object storage, and external providers.
- For each role, state where it runs, what triggers it, what it consumes,
  what it produces, and which system it hands work to.
- Explicitly identify the queue, who enqueues jobs, who claims them,
  and who starts workers. Define unfamiliar terms briefly.
- Establish the deployment topology before discussing function signatures,
  schemas, batching, or implementation details.
- Clearly distinguish the current deployed system from a proposed design.
- Prefer one clear diagram and a short role table over a long narrative.
  Do not collapse distinct deployment roles into one hosting-platform box.
