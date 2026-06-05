import * as pulumi from "@pulumi/pulumi";
import * as railway from "@pulumi/railway";

const cfg = new pulumi.Config();

const railwayToken = cfg.getSecret("railwayToken");
const provider = railwayToken
  ? new railway.Provider("railway", { token: railwayToken })
  : undefined;
const opts: pulumi.CustomResourceOptions = provider ? { provider } : {};

const projectId = cfg.get("projectId");
const environmentIdFromConfig = cfg.get("environmentId");
const environmentName = cfg.get("environmentName") ?? pulumi.getStack();

let project: railway.Project;
let environmentId: pulumi.Input<string>;

if (projectId) {
  if (!environmentIdFromConfig) {
    throw new Error("pufferfs:environmentId is required when pufferfs:projectId is set");
  }
  project = railway.Project.get("project", projectId, undefined, opts);
  environmentId = environmentIdFromConfig;
} else {
  project = new railway.Project(
    "project",
    {
      name: cfg.get("projectName") ?? "pufferfs",
      workspaceId: cfg.require("workspaceId"),
      defaultEnvironment: { name: environmentName },
      private: true,
    },
    opts,
  );
  environmentId = project.defaultEnvironment.apply((env) => {
    if (!env.id) {
      throw new Error("Railway did not return a default environment id");
    }
    return env.id;
  });
}

const sourceRepo = cfg.get("sourceRepo") ?? "suhjohn/pufferfs";
const sourceRepoBranch = cfg.get("sourceRepoBranch") ?? "main";
const appRootDirectory = cfg.get("appRootDirectory");
const region = cfg.get("region");
const natsClusterName = cfg.get("natsClusterName") ?? "pufferfs";
const natsVolumeSizeMb = cfg.getNumber("natsVolumeSizeMb") ?? 50_000;
const natsNodes = ["nats-1", "nats-2", "nats-3"] as const;
const natsRoutes = natsNodes.map((name) => `nats://${name}.railway.internal:6222`).join(",");
const natsURL = natsNodes.map((name) => `nats://${name}.railway.internal:4222`).join(",");

type Vars = Record<string, pulumi.Input<string>>;

function serviceRegions(replicas: number): pulumi.Input<railway.types.input.ServiceRegion>[] | undefined {
  if (!region && replicas === 1) {
    return undefined;
  }
  return [region ? { region, numReplicas: replicas } : { numReplicas: replicas }];
}

function appService(name: string, replicas: number): railway.Service {
  return new railway.Service(
    name,
    {
      name,
      projectId: project.id,
      sourceRepo,
      sourceRepoBranch,
      rootDirectory: appRootDirectory,
      regions: serviceRegions(replicas),
    },
    opts,
  );
}

function setVariables(serviceName: string, service: railway.Service, vars: Vars): void {
  for (const [name, value] of Object.entries(vars)) {
    const resourceName = `${serviceName}-${name.toLowerCase().replaceAll("_", "-")}`;
    new railway.Variable(
      resourceName,
      {
        environmentId,
        serviceId: service.id,
        name,
        value,
      },
      opts,
    );
  }
}

const natsServices = natsNodes.map((name) => {
  const service = new railway.Service(
    name,
    {
      name,
      projectId: project.id,
      sourceRepo,
      sourceRepoBranch,
      rootDirectory: "deploy/nats",
      regions: serviceRegions(1),
      volume: {
        name: `${name}-jetstream`,
        mountPath: "/data",
        size: natsVolumeSizeMb,
      },
    },
    opts,
  );

  setVariables(name, service, {
    NATS_SERVER_NAME: name,
    NATS_CLUSTER_NAME: natsClusterName,
    NATS_STORE_DIR: "/data",
    NATS_ROUTES: natsRoutes,
  });

  return service;
});

const apiReplicas = cfg.getNumber("apiReplicas") ?? 1;
const api = appService("api", apiReplicas);

const commonAppVars: Vars = {
  DATABASE_URL: cfg.get("databaseUrlRef") ?? "${{shared.DATABASE_URL}}",
  AWS_ENDPOINT_URL: cfg.require("awsEndpointUrl"),
  AWS_BUCKET_NAME: cfg.require("awsBucketName"),
  AWS_ACCESS_KEY_ID: cfg.requireSecret("awsAccessKeyId"),
  AWS_SECRET_ACCESS_KEY: cfg.requireSecret("awsSecretAccessKey"),
  TURBOPUFFER_API_KEY: cfg.requireSecret("turbopufferApiKey"),
  MODAL_CHUNK_ENDPOINT: cfg.require("modalChunkEndpoint"),
  MODAL_EMBED_ENDPOINT: cfg.require("modalEmbedEndpoint"),
  MODAL_QUERY_EMBED_ENDPOINT: cfg.require("modalQueryEmbedEndpoint"),
  MODAL_CHUNK_SHARD_ENDPOINT: cfg.require("modalChunkShardEndpoint"),
  MODAL_EMBED_SHARD_ENDPOINT: cfg.require("modalEmbedShardEndpoint"),
  MODAL_INDEX_SHARD_ENDPOINT: cfg.require("modalIndexShardEndpoint"),
  NATS_URL: natsURL,
  PUFFERFS_QUEUE_REPLICAS: natsNodes.length.toString(),
};

const tpNamespaceShards = cfg.get("tpNamespaceShards");
if (tpNamespaceShards) {
  commonAppVars.PUFFERFS_TP_NAMESPACE_SHARDS = tpNamespaceShards;
}

setVariables("api", api, {
  ...commonAppVars,
  JWT_SECRET: cfg.requireSecret("jwtSecret"),
});

const adminKeyHash = cfg.getSecret("adminKeyHash");
if (adminKeyHash) {
  setVariables("api-admin", api, {
    PUFFERFS_ADMIN_KEY_HASH: adminKeyHash,
  });
}

const apiSubdomain = cfg.get("apiSubdomain");
const apiDomain = apiSubdomain
  ? new railway.ServiceDomain(
      "api-domain",
      {
        environmentId,
        serviceId: api.id,
        subdomain: apiSubdomain,
      },
      opts,
    )
  : undefined;

const workerDefaults: Record<string, number> = {
  chunk: 16,
  embed: 8,
  index: 16,
  commit: 2,
  cleanup: 4,
};

const workerReplicas = cfg.getNumber("workerReplicas") ?? 1;
const workers = Object.entries(workerDefaults).map(([stage, defaultConcurrency]) => {
  const serviceName = `worker-${stage}`;
  const service = appService(serviceName, workerReplicas);
  setVariables(serviceName, service, {
    ...commonAppVars,
    PUFFERFS_PROCESS: "worker",
    PUFFERFS_WORKER_STAGE: stage,
    PUFFERFS_WORKER_CONCURRENCY: (
      cfg.getNumber(`worker${stage[0].toUpperCase()}${stage.slice(1)}Concurrency`) ??
      defaultConcurrency
    ).toString(),
  });
  return service;
});

export const railwayProjectId = project.id;
export const railwayEnvironmentId = environmentId;
export const apiServiceId = api.id;
export const apiURL = apiDomain?.domain;
export const natsServiceIds = natsServices.map((service) => service.id);
export const workerServiceIds = workers.map((service) => service.id);
