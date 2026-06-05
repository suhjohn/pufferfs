# PufferFS Pulumi Deployment

This stack manages the Railway runtime shape for PufferFS:

- 3 NATS JetStream services, each with its own persistent volume.
- 1 API service.
- 5 worker services: `chunk`, `embed`, `index`, `commit`, and `cleanup`.
- Per-service Railway variables.
- Optional public Railway domain for the API service.

It intentionally does not create Postgres or object storage. Use the existing
Railway shared `DATABASE_URL` and pass the S3-compatible bucket credentials as
stack config.

## Structure

```text
infra/pulumi/
  Pulumi.yaml                  # project + generated Railway provider package
  Pulumi.prod.yaml.example     # example production config
  index.ts                     # Railway services, variables, volumes, domain
  sdks/railway/                # generated Pulumi SDK for the Railway provider
```

The app services deploy from `suhjohn/pufferfs` by default. The NATS services use
the `deploy/nats` root directory, which builds the NATS image and starts
JetStream with cluster routes.

## Stacks

Use one Pulumi stack per environment:

```sh
cd infra/pulumi
npm install

pulumi stack init dev
pulumi stack init prod
```

For an existing Railway project, set the project and environment IDs:

```sh
pulumi config set pufferfs:projectId 9d0532da-8a53-42e2-8ac7-313fcb1fd09f
pulumi config set pufferfs:environmentId 6de44940-bc95-4c51-81e0-cfb3bfe1ef27
```

For a new Railway project, omit those IDs and set:

```sh
pulumi config set pufferfs:workspaceId <railway-workspace-id>
pulumi config set pufferfs:projectName pufferfs
pulumi config set pufferfs:environmentName production
```

## Required Config

```sh
pulumi config set pufferfs:sourceRepo suhjohn/pufferfs
pulumi config set pufferfs:sourceRepoBranch main
pulumi config set pufferfs:databaseUrlRef '${{shared.DATABASE_URL}}'

pulumi config set pufferfs:awsEndpointUrl <s3-endpoint-url>
pulumi config set pufferfs:awsBucketName <bucket-name>
pulumi config set --secret pufferfs:awsAccessKeyId <access-key-id>
pulumi config set --secret pufferfs:awsSecretAccessKey <secret-access-key>

pulumi config set --secret pufferfs:jwtSecret "$(openssl rand -base64 32)"
pulumi config set --secret pufferfs:turbopufferApiKey <tp-api-key>

pulumi config set pufferfs:modalChunkEndpoint https://...chunk-file-endpoint.modal.run
pulumi config set pufferfs:modalEmbedEndpoint https://...embed-chunks-endpoint.modal.run
pulumi config set pufferfs:modalQueryEmbedEndpoint https://...embed-query-endpoint.modal.run
pulumi config set pufferfs:modalChunkShardEndpoint https://...chunk-shard-endpoint.modal.run
pulumi config set pufferfs:modalEmbedShardEndpoint https://...embed-shard-endpoint.modal.run
pulumi config set pufferfs:modalIndexShardEndpoint https://...index-shard-endpoint.modal.run
```

Use either `RAILWAY_TOKEN` in the shell or a Pulumi secret:

```sh
export RAILWAY_TOKEN=...
# or
pulumi config set --secret pufferfs:railwayToken <railway-token>
```

## Optional Config

```sh
pulumi config set pufferfs:apiSubdomain pufferfs-prod
pulumi config set pufferfs:natsVolumeSizeMb 50000
pulumi config set pufferfs:apiReplicas 1
pulumi config set pufferfs:workerReplicas 1
pulumi config set pufferfs:tpNamespaceShards 4
pulumi config set --secret pufferfs:adminKeyHash <sha256-admin-key-hash>
```

Worker stage concurrency can be overridden independently:

```sh
pulumi config set pufferfs:workerChunkConcurrency 16
pulumi config set pufferfs:workerEmbedConcurrency 8
pulumi config set pufferfs:workerIndexConcurrency 16
pulumi config set pufferfs:workerCommitConcurrency 2
pulumi config set pufferfs:workerCleanupConcurrency 4
```

## Deploy

```sh
npm run build
pulumi preview
pulumi up
```

If matching services already exist from a manual Railway deployment, delete them
or import them before `pulumi up`; otherwise Pulumi will try to create a second
set with the same names.
