import * as aws from "@pulumi/aws";
import * as docker from "@pulumi/docker";
import * as pulumi from "@pulumi/pulumi";

const cfg = new pulumi.Config();
const stack = pulumi.getStack();
const project = cfg.get("projectName") ?? "pufferfs";
const name = (suffix: string) => `${project}-${stack}-${suffix}`;

const vpcCidr = cfg.get("vpcCidr") ?? "10.80.0.0/16";
const azs = cfg.getObject<string[]>("availabilityZones") ?? ["us-east-1a", "us-east-1b"];
if (azs.length < 2) {
  throw new Error("pufferfs:availabilityZones must include at least two AZs");
}

const containerPort = cfg.getNumber("containerPort") ?? 8080;
const cpu = cfg.getNumber("taskCpu") ?? 1024;
const memory = cfg.getNumber("taskMemory") ?? 2048;
const imageTag = cfg.get("imageTag") ?? stack;

// Optional payment surface. When false (default) no Stripe config is required
// and the frontend ships with billing hidden (VITE_ENABLE_BILLING=false).
const enableBilling = cfg.getBoolean("enableBilling") ?? false;
// Public URL of the web app; passed to the API for OAuth redirects + CORS.
const frontendUrl = cfg.get("frontendUrl");
const natsImage = cfg.get("natsImage") ?? "nats:2-alpine";
const natsClusterName = cfg.get("natsClusterName") ?? "pufferfs";
const natsNodes = ["nats-1", "nats-2", "nats-3"] as const;
const natsDnsZone = `${project}-${stack}.local`;
const natsRoutes = natsNodes.map((node) => `nats://${node}.${natsDnsZone}:6222`).join(",");
const natsURL = natsNodes.map((node) => `nats://${node}.${natsDnsZone}:4222`).join(",");

const tags = {
  Project: project,
  Stack: stack,
  ManagedBy: "pulumi",
};

const vpc = new aws.ec2.Vpc(name("vpc"), {
  cidrBlock: vpcCidr,
  enableDnsHostnames: true,
  enableDnsSupport: true,
  tags: { ...tags, Name: name("vpc") },
});

const internetGateway = new aws.ec2.InternetGateway(name("igw"), {
  vpcId: vpc.id,
  tags: { ...tags, Name: name("igw") },
});

const publicSubnets = azs.map((az, i) =>
  new aws.ec2.Subnet(name(`public-${i + 1}`), {
    vpcId: vpc.id,
    availabilityZone: az,
    cidrBlock: `10.80.${i}.0/24`,
    mapPublicIpOnLaunch: true,
    tags: { ...tags, Name: name(`public-${i + 1}`) },
  }),
);

const privateSubnets = azs.map((az, i) =>
  new aws.ec2.Subnet(name(`private-${i + 1}`), {
    vpcId: vpc.id,
    availabilityZone: az,
    cidrBlock: `10.80.${i + 10}.0/24`,
    tags: { ...tags, Name: name(`private-${i + 1}`) },
  }),
);

const publicRouteTable = new aws.ec2.RouteTable(name("public-rt"), {
  vpcId: vpc.id,
  routes: [{ cidrBlock: "0.0.0.0/0", gatewayId: internetGateway.id }],
  tags: { ...tags, Name: name("public-rt") },
});

publicSubnets.forEach((subnet, i) => {
  new aws.ec2.RouteTableAssociation(name(`public-rta-${i + 1}`), {
    subnetId: subnet.id,
    routeTableId: publicRouteTable.id,
  });
});

const natEip = new aws.ec2.Eip(name("nat-eip"), {
  domain: "vpc",
  tags: { ...tags, Name: name("nat-eip") },
});

const natGateway = new aws.ec2.NatGateway(
  name("nat"),
  {
    allocationId: natEip.id,
    subnetId: publicSubnets[0].id,
    tags: { ...tags, Name: name("nat") },
  },
  { dependsOn: [internetGateway] },
);

const privateRouteTable = new aws.ec2.RouteTable(name("private-rt"), {
  vpcId: vpc.id,
  routes: [{ cidrBlock: "0.0.0.0/0", natGatewayId: natGateway.id }],
  tags: { ...tags, Name: name("private-rt") },
});

privateSubnets.forEach((subnet, i) => {
  new aws.ec2.RouteTableAssociation(name(`private-rta-${i + 1}`), {
    subnetId: subnet.id,
    routeTableId: privateRouteTable.id,
  });
});

const appRepo = new aws.ecr.Repository(name("app"), {
  forceDelete: true,
  imageScanningConfiguration: { scanOnPush: true },
  tags,
});

new aws.ecr.LifecyclePolicy(name("app-lifecycle"), {
  repository: appRepo.name,
  policy: JSON.stringify({
    rules: [
      {
        rulePriority: 1,
        description: "Keep the last 20 images",
        selection: {
          tagStatus: "any",
          countType: "imageCountMoreThan",
          countNumber: 20,
        },
        action: { type: "expire" },
      },
    ],
  }),
});

const authToken = aws.ecr.getAuthorizationTokenOutput({ registryId: appRepo.registryId });
const appImage = new docker.Image(name("app-image"), {
  imageName: pulumi.interpolate`${appRepo.repositoryUrl}:${imageTag}`,
  build: {
    context: "../../",
    dockerfile: "../../Dockerfile",
    platform: "linux/amd64",
  },
  registry: {
    server: appRepo.repositoryUrl.apply((url) => url.split("/")[0]),
    username: authToken.userName,
    password: authToken.password,
  },
});

const bucket = new aws.s3.BucketV2(name("artifacts"), {
  forceDestroy: cfg.getBoolean("forceDestroyBucket") ?? false,
  tags,
});

new aws.s3.BucketPublicAccessBlock(name("artifacts-public-access"), {
  bucket: bucket.id,
  blockPublicAcls: true,
  blockPublicPolicy: true,
  ignorePublicAcls: true,
  restrictPublicBuckets: true,
});

new aws.s3.BucketServerSideEncryptionConfigurationV2(name("artifacts-encryption"), {
  bucket: bucket.id,
  rules: [
    {
      applyServerSideEncryptionByDefault: {
        sseAlgorithm: "AES256",
      },
    },
  ],
});

// --- Frontend: TanStack Start prerendered to static, on S3 + CloudFront -----
// The web build (web/dist) is a plain folder of static files. Upload happens
// out of band (`aws s3 sync` in CI); see web/README.md. CloudFront serves it
// over HTTPS and rewrites 403/404 -> /index.html so SPA deep links resolve.
const webBucket = new aws.s3.BucketV2(name("web"), {
  forceDestroy: true,
  tags,
});

new aws.s3.BucketPublicAccessBlock(name("web-public-access"), {
  bucket: webBucket.id,
  blockPublicAcls: true,
  blockPublicPolicy: true,
  ignorePublicAcls: true,
  restrictPublicBuckets: true,
});

// Optional custom domain for the frontend. DNS is managed in Cloudflare, so we
// only create the ACM cert here and export its validation CNAME + the
// CloudFront domain for you to add as records. CloudFront can only attach an
// *issued* cert, so the attach (aliases + viewerCertificate) is gated on
// webHttpsReady: first `pulumi up` with just webDomain creates the cert and
// outputs the validation record; add it in Cloudflare, wait for ISSUED, then
// set webHttpsReady=true and `pulumi up` again.
const webDomain = cfg.get("webDomain");
const webHttpsReady = cfg.getBoolean("webHttpsReady") ?? false;

// CloudFront certs must live in us-east-1 regardless of the stack region.
const usEast1 = new aws.Provider("us-east-1", { region: "us-east-1" });

let webCert: aws.acm.Certificate | undefined;
let webCertArn: pulumi.Input<string> | undefined;
if (webDomain) {
  webCert = new aws.acm.Certificate(
    name("web-cert"),
    { domainName: webDomain, validationMethod: "DNS", tags },
    { provider: usEast1 },
  );
  if (webHttpsReady) {
    const webCertValidated = new aws.acm.CertificateValidation(
      name("web-cert-validated"),
      { certificateArn: webCert.arn },
      { provider: usEast1 },
    );
    webCertArn = webCertValidated.certificateArn;
  }
}
const useWebCustomCert = Boolean(webDomain && webHttpsReady && webCertArn);

const webOac = new aws.cloudfront.OriginAccessControl(name("web-oac"), {
  originAccessControlOriginType: "s3",
  signingBehavior: "always",
  signingProtocol: "sigv4",
});

const webDistribution = new aws.cloudfront.Distribution(name("web-cdn"), {
  enabled: true,
  // SPA mode emits a single route-agnostic _shell.html served for every path.
  defaultRootObject: "_shell.html",
  aliases: useWebCustomCert ? [webDomain!] : undefined,
  origins: [
    {
      originId: "web-s3",
      domainName: webBucket.bucketRegionalDomainName,
      originAccessControlId: webOac.id,
    },
  ],
  defaultCacheBehavior: {
    targetOriginId: "web-s3",
    viewerProtocolPolicy: "redirect-to-https",
    allowedMethods: ["GET", "HEAD", "OPTIONS"],
    cachedMethods: ["GET", "HEAD"],
    compress: true,
    // AWS managed "CachingOptimized" cache policy.
    cachePolicyId: "658327ea-f89d-4fab-a63d-7e88639e58f6",
  },
  customErrorResponses: [
    { errorCode: 403, responseCode: 200, responsePagePath: "/_shell.html" },
    { errorCode: 404, responseCode: 200, responsePagePath: "/_shell.html" },
  ],
  priceClass: "PriceClass_100",
  restrictions: { geoRestriction: { restrictionType: "none" } },
  viewerCertificate: useWebCustomCert
    ? {
        acmCertificateArn: webCertArn,
        sslSupportMethod: "sni-only",
        minimumProtocolVersion: "TLSv1.2_2021",
      }
    : { cloudfrontDefaultCertificate: true },
  tags,
});

new aws.s3.BucketPolicy(name("web-policy"), {
  bucket: webBucket.id,
  policy: pulumi
    .all([webBucket.arn, webDistribution.arn])
    .apply(([bucketArn, distArn]) =>
      JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Principal: { Service: "cloudfront.amazonaws.com" },
            Action: "s3:GetObject",
            Resource: `${bucketArn}/*`,
            Condition: { StringEquals: { "AWS:SourceArn": distArn } },
          },
        ],
      }),
    ),
});

const cluster = new aws.ecs.Cluster(name("cluster"), {
  tags,
});

const logGroupName = `/ecs/${project}/${stack}`;
const logGroup = new aws.cloudwatch.LogGroup(name("logs"), {
  name: logGroupName,
  retentionInDays: cfg.getNumber("logRetentionDays") ?? 14,
  tags,
});

const executionRole = new aws.iam.Role(name("ecs-execution-role"), {
  assumeRolePolicy: aws.iam.assumeRolePolicyForPrincipal({
    Service: "ecs-tasks.amazonaws.com",
  }),
  tags,
});

new aws.iam.RolePolicyAttachment(name("ecs-execution-managed-policy"), {
  role: executionRole.name,
  policyArn: aws.iam.ManagedPolicy.AmazonECSTaskExecutionRolePolicy,
});

const taskRole = new aws.iam.Role(name("ecs-task-role"), {
  assumeRolePolicy: aws.iam.assumeRolePolicyForPrincipal({
    Service: "ecs-tasks.amazonaws.com",
  }),
  tags,
});

new aws.iam.RolePolicy(name("ecs-task-policy"), {
  role: taskRole.id,
  policy: pulumi
    .all([bucket.arn])
    .apply(([bucketArn]) =>
      JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Action: ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"],
            Resource: [bucketArn, `${bucketArn}/*`],
          },
          {
            Effect: "Allow",
            Action: ["secretsmanager:GetSecretValue"],
            Resource: "*",
          },
        ],
      }),
    ),
});

new aws.iam.RolePolicy(name("ecs-execution-secrets-policy"), {
  role: executionRole.id,
  policy: JSON.stringify({
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Action: ["secretsmanager:GetSecretValue"],
        Resource: "*",
      },
    ],
  }),
});

const albSg = new aws.ec2.SecurityGroup(name("alb-sg"), {
  vpcId: vpc.id,
  ingress: [
    {
      protocol: "tcp",
      fromPort: 80,
      toPort: 80,
      cidrBlocks: ["0.0.0.0/0"],
    },
    {
      protocol: "tcp",
      fromPort: 443,
      toPort: 443,
      cidrBlocks: ["0.0.0.0/0"],
    },
  ],
  egress: [
    {
      protocol: "-1",
      fromPort: 0,
      toPort: 0,
      cidrBlocks: ["0.0.0.0/0"],
    },
  ],
  tags: { ...tags, Name: name("alb-sg") },
});

const appSg = new aws.ec2.SecurityGroup(name("app-sg"), {
  vpcId: vpc.id,
  ingress: [
    {
      protocol: "tcp",
      fromPort: containerPort,
      toPort: containerPort,
      securityGroups: [albSg.id],
    },
  ],
  egress: [
    {
      protocol: "-1",
      fromPort: 0,
      toPort: 0,
      cidrBlocks: ["0.0.0.0/0"],
    },
  ],
  tags: { ...tags, Name: name("app-sg") },
});

const natsSg = new aws.ec2.SecurityGroup(name("nats-sg"), {
  vpcId: vpc.id,
  ingress: [
    {
      protocol: "tcp",
      fromPort: 4222,
      toPort: 4222,
      securityGroups: [appSg.id],
    },
    {
      protocol: "tcp",
      fromPort: 6222,
      toPort: 6222,
      self: true,
    },
    {
      protocol: "tcp",
      fromPort: 2049,
      toPort: 2049,
      securityGroups: [appSg.id],
    },
    {
      protocol: "tcp",
      fromPort: 2049,
      toPort: 2049,
      self: true,
    },
  ],
  egress: [
    {
      protocol: "-1",
      fromPort: 0,
      toPort: 0,
      cidrBlocks: ["0.0.0.0/0"],
    },
  ],
  tags: { ...tags, Name: name("nats-sg") },
});

const fileSystem = new aws.efs.FileSystem(name("nats-efs"), {
  encrypted: true,
  tags,
});

const mountTargets = privateSubnets.map((subnet, i) =>
  new aws.efs.MountTarget(name(`nats-efs-mt-${i + 1}`), {
    fileSystemId: fileSystem.id,
    subnetId: subnet.id,
    securityGroups: [natsSg.id],
  }),
);

const natsAccessPoints = natsNodes.map(
  (node) =>
    new aws.efs.AccessPoint(name(`${node}-ap`), {
      fileSystemId: fileSystem.id,
      posixUser: {
        uid: 1000,
        gid: 1000,
      },
      rootDirectory: {
        path: `/${node}`,
        creationInfo: {
          ownerUid: 1000,
          ownerGid: 1000,
          permissions: "700",
        },
      },
      tags: { ...tags, Name: name(`${node}-ap`) },
    }),
);

new aws.iam.RolePolicy(name("ecs-efs-client-policy"), {
  role: taskRole.id,
  policy: pulumi
    .all([fileSystem.arn, natsAccessPoints.map((ap) => ap.arn)])
    .apply(([fileSystemArn, accessPointArns]) =>
      JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Action: [
              "elasticfilesystem:ClientMount",
              "elasticfilesystem:ClientWrite",
              "elasticfilesystem:DescribeMountTargets",
            ],
            Resource: [fileSystemArn, ...accessPointArns],
          },
        ],
      }),
    ),
});

const namespace = new aws.servicediscovery.PrivateDnsNamespace(name("service-discovery"), {
  name: natsDnsZone,
  vpc: vpc.id,
  tags,
});

const natsDiscoveryServices = natsNodes.map(
  (node) =>
    new aws.servicediscovery.Service(name(`${node}-discovery`), {
      name: node,
      dnsConfig: {
        namespaceId: namespace.id,
        dnsRecords: [{ ttl: 10, type: "A" }],
        routingPolicy: "MULTIVALUE",
      },
      tags,
    }),
);

const secretValues: Record<string, pulumi.Input<string>> = {
  DATABASE_URL: cfg.requireSecret("databaseUrl"),
  JWT_SECRET: cfg.requireSecret("jwtSecret"),
  TURBOPUFFER_API_KEY: cfg.requireSecret("turbopufferApiKey"),
};

const adminKeyHash = cfg.getSecret("adminKeyHash");
if (adminKeyHash) {
  secretValues.PUFFERFS_ADMIN_KEY_HASH = adminKeyHash;
}

// Google OAuth client secret (paired with the googleClientId plain config).
const googleClientSecret = cfg.getSecret("googleClientSecret");
if (googleClientSecret) {
  secretValues.GOOGLE_CLIENT_SECRET = googleClientSecret;
}

// Stripe secrets only exist for deployments that opted into billing.
if (enableBilling) {
  secretValues.STRIPE_SECRET_KEY = cfg.requireSecret("stripeSecretKey");
  secretValues.STRIPE_WEBHOOK_SECRET = cfg.requireSecret("stripeWebhookSecret");
}

const secrets = Object.entries(secretValues).map(([key, value]) => {
  const secret = new aws.secretsmanager.Secret(name(key.toLowerCase().replaceAll("_", "-")), {
    tags,
  });
  new aws.secretsmanager.SecretVersion(name(`${key.toLowerCase().replaceAll("_", "-")}-value`), {
    secretId: secret.id,
    secretString: value,
  });
  return { name: key, valueFrom: secret.arn };
});

const appEnv = [
  { name: "PORT", value: containerPort.toString() },
  { name: "AWS_BUCKET_NAME", value: bucket.bucket },
  { name: "AWS_REGION", value: aws.config.region ?? "us-east-1" },
  { name: "AWS_ENDPOINT_URL", value: "" },
  { name: "NATS_URL", value: natsURL },
  { name: "PUFFERFS_QUEUE_REPLICAS", value: natsNodes.length.toString() },
  { name: "MODAL_CHUNK_ENDPOINT", value: cfg.require("modalChunkEndpoint") },
  { name: "MODAL_EMBED_ENDPOINT", value: cfg.require("modalEmbedEndpoint") },
  { name: "MODAL_QUERY_EMBED_ENDPOINT", value: cfg.require("modalQueryEmbedEndpoint") },
  { name: "MODAL_CHUNK_SHARD_ENDPOINT", value: cfg.require("modalChunkShardEndpoint") },
  { name: "MODAL_EMBED_SHARD_ENDPOINT", value: cfg.require("modalEmbedShardEndpoint") },
  { name: "MODAL_INDEX_SHARD_ENDPOINT", value: cfg.require("modalIndexShardEndpoint") },
  { name: "ENABLE_BILLING", value: enableBilling ? "true" : "false" },
];

const cliLatestVersion = cfg.get("cliLatestVersion");
if (cliLatestVersion) {
  appEnv.push({ name: "PUFFERFS_CLI_LATEST_VERSION", value: cliLatestVersion });
}
const cliMinVersion = cfg.get("cliMinVersion");
if (cliMinVersion) {
  appEnv.push({ name: "PUFFERFS_CLI_MIN_VERSION", value: cliMinVersion });
}
const cliDownloadBaseUrl = cfg.get("cliDownloadBaseUrl");
if (cliDownloadBaseUrl) {
  appEnv.push({ name: "PUFFERFS_CLI_DOWNLOAD_BASE_URL", value: cliDownloadBaseUrl });
}

if (frontendUrl) {
  appEnv.push({ name: "FRONTEND_URL", value: frontendUrl });
}

// Session cookie domain (e.g. ".pufferfs.com") so the app + api subdomains
// share the auth cookie.
const cookieDomain = cfg.get("cookieDomain");
if (cookieDomain) {
  appEnv.push({ name: "COOKIE_DOMAIN", value: cookieDomain });
}

// Google OAuth (client secret is stored in Secrets Manager via secretValues).
const googleClientId = cfg.get("googleClientId");
if (googleClientId) {
  appEnv.push({ name: "GOOGLE_CLIENT_ID", value: googleClientId });
}
const oauthRedirectUrl = cfg.get("oauthRedirectUrl");
if (oauthRedirectUrl) {
  appEnv.push({ name: "OAUTH_REDIRECT_URL", value: oauthRedirectUrl });
}

// Stripe price id (non-secret) when billing is enabled.
if (enableBilling) {
  const stripePriceId = cfg.get("stripePriceId");
  if (stripePriceId) {
    appEnv.push({ name: "STRIPE_PRICE_ID", value: stripePriceId });
  }
}

const tpNamespaceShards = cfg.get("tpNamespaceShards");
if (tpNamespaceShards) {
  appEnv.push({ name: "PUFFERFS_TP_NAMESPACE_SHARDS", value: tpNamespaceShards });
}

function logConfig(streamPrefix: string) {
  return {
    logDriver: "awslogs",
    options: {
      "awslogs-group": logGroupName,
      "awslogs-region": aws.config.region ?? "us-east-1",
      "awslogs-stream-prefix": streamPrefix,
    },
  };
}

function appTaskDefinition(
  resourceName: string,
  containerName: string,
  extraEnv: { name: string; value: pulumi.Input<string> }[],
  portMappings?: { containerPort: number; hostPort?: number; protocol?: string }[],
) {
  return new aws.ecs.TaskDefinition(resourceName, {
    family: name(resourceName),
    requiresCompatibilities: ["FARGATE"],
    networkMode: "awsvpc",
    cpu: cpu.toString(),
    memory: memory.toString(),
    executionRoleArn: executionRole.arn,
    taskRoleArn: taskRole.arn,
    containerDefinitions: pulumi
      .all([appImage.imageName, appEnv, secrets])
      .apply(([image, env, secretDefs]) =>
        JSON.stringify([
          {
            name: containerName,
            image,
            essential: true,
            environment: [...env, ...extraEnv],
            secrets: secretDefs,
            portMappings,
            logConfiguration: logConfig(containerName),
          },
        ]),
      ),
    tags,
  });
}

const alb = new aws.lb.LoadBalancer(name("api-alb"), {
  loadBalancerType: "application",
  internal: false,
  subnets: publicSubnets.map((subnet) => subnet.id),
  securityGroups: [albSg.id],
  tags,
});

const targetGroup = new aws.lb.TargetGroup(name("api-tg"), {
  port: containerPort,
  protocol: "HTTP",
  targetType: "ip",
  vpcId: vpc.id,
  healthCheck: {
    path: cfg.get("healthCheckPath") ?? "/health",
    matcher: "200-399",
  },
  tags,
});

// HTTPS for the API. DNS lives in Cloudflare, so Pulumi only creates the ACM
// cert and exports its validation CNAME + the ALB hostname for you to add as
// records. The :443 listener can only attach an *issued* cert, so it is gated
// on apiHttpsReady (same two-phase flow as the web cert above): first
// `pulumi up` with apiDomain creates the cert + outputs; add the validation
// CNAME and the `api` CNAME (→ apiAlbHostname, DNS-only) in Cloudflare, wait
// for ISSUED, then set apiHttpsReady=true and `pulumi up` again.
const apiDomain = cfg.get("apiDomain");
const apiHttpsReady = cfg.getBoolean("apiHttpsReady") ?? false;

let apiCert: aws.acm.Certificate | undefined;
if (apiDomain) {
  apiCert = new aws.acm.Certificate(name("api-cert"), {
    domainName: apiDomain,
    validationMethod: "DNS",
    tags,
  });
}

if (apiDomain && apiHttpsReady && apiCert) {
  // Waits until ACM reports the cert ISSUED (validation CNAME present in
  // Cloudflare), then serves HTTPS and redirects :80 → :443.
  const apiCertValidated = new aws.acm.CertificateValidation(name("api-cert-validated"), {
    certificateArn: apiCert.arn,
  });

  new aws.lb.Listener(name("api-https"), {
    loadBalancerArn: alb.arn,
    port: 443,
    protocol: "HTTPS",
    sslPolicy: "ELBSecurityPolicy-TLS13-1-2-2021-06",
    certificateArn: apiCertValidated.certificateArn,
    defaultActions: [{ type: "forward", targetGroupArn: targetGroup.arn }],
  });

  new aws.lb.Listener(name("api-http"), {
    loadBalancerArn: alb.arn,
    port: 80,
    protocol: "HTTP",
    defaultActions: [
      {
        type: "redirect",
        redirect: { port: "443", protocol: "HTTPS", statusCode: "HTTP_301" },
      },
    ],
  });
} else {
  new aws.lb.Listener(name("api-http"), {
    loadBalancerArn: alb.arn,
    port: 80,
    protocol: "HTTP",
    defaultActions: [{ type: "forward", targetGroupArn: targetGroup.arn }],
  });
}

const apiTask = appTaskDefinition(
  "api-task",
  "api",
  [],
  [{ containerPort, hostPort: containerPort, protocol: "tcp" }],
);

const apiService = new aws.ecs.Service(name("api"), {
  cluster: cluster.arn,
  taskDefinition: apiTask.arn,
  desiredCount: cfg.getNumber("apiDesiredCount") ?? 2,
  launchType: "FARGATE",
  waitForSteadyState: false,
  networkConfiguration: {
    subnets: privateSubnets.map((subnet) => subnet.id),
    securityGroups: [appSg.id],
    assignPublicIp: false,
  },
  loadBalancers: [
    {
      targetGroupArn: targetGroup.arn,
      containerName: "api",
      containerPort,
    },
  ],
  tags,
});

const workerDefaults: Record<string, number> = {
  chunk: 16,
  embed: 8,
  index: 16,
  commit: 2,
  cleanup: 4,
};

const workerServices = Object.entries(workerDefaults).map(([stage, defaultConcurrency]) => {
  const serviceName = `worker-${stage}`;
  const concurrency =
    cfg.getNumber(`worker${stage[0].toUpperCase()}${stage.slice(1)}Concurrency`) ??
    defaultConcurrency;
  const task = appTaskDefinition(`${serviceName}-task`, serviceName, [
    { name: "PUFFERFS_PROCESS", value: "worker" },
    { name: "PUFFERFS_WORKER_STAGE", value: stage },
    { name: "PUFFERFS_WORKER_CONCURRENCY", value: concurrency.toString() },
  ]);

  return new aws.ecs.Service(name(serviceName), {
    cluster: cluster.arn,
    taskDefinition: task.arn,
    desiredCount: cfg.getNumber(`${serviceName}DesiredCount`) ?? 1,
    launchType: "FARGATE",
    waitForSteadyState: false,
    networkConfiguration: {
      subnets: privateSubnets.map((subnet) => subnet.id),
      securityGroups: [appSg.id],
      assignPublicIp: false,
    },
    tags,
  });
});

const natsServices = natsNodes.map((node, i) => {
  const task = new aws.ecs.TaskDefinition(name(`${node}-task`), {
    family: name(`${node}-task`),
    requiresCompatibilities: ["FARGATE"],
    networkMode: "awsvpc",
    cpu: "512",
    memory: "1024",
    executionRoleArn: executionRole.arn,
    taskRoleArn: taskRole.arn,
    volumes: [
      {
        name: "nats-data",
        efsVolumeConfiguration: {
          fileSystemId: fileSystem.id,
          transitEncryption: "ENABLED",
          authorizationConfig: {
            accessPointId: natsAccessPoints[i].id,
            iam: "ENABLED",
          },
        },
      },
    ],
    containerDefinitions: JSON.stringify([
      {
        name: node,
        image: natsImage,
        essential: true,
        command: [
          "-js",
          "-sd",
          "/data",
          "--server_name",
          node,
          "--cluster_name",
          natsClusterName,
          "-p",
          "4222",
          "-m",
          "8222",
          "-cluster",
          "nats://0.0.0.0:6222",
          "-routes",
          natsRoutes,
        ],
        portMappings: [
          { containerPort: 4222, protocol: "tcp" },
          { containerPort: 6222, protocol: "tcp" },
          { containerPort: 8222, protocol: "tcp" },
        ],
        mountPoints: [{ sourceVolume: "nats-data", containerPath: "/data" }],
        logConfiguration: logConfig(node),
      },
    ]),
    tags,
  });

  return new aws.ecs.Service(
    name(node),
    {
      cluster: cluster.arn,
      taskDefinition: task.arn,
      desiredCount: 1,
      launchType: "FARGATE",
      waitForSteadyState: false,
      networkConfiguration: {
        subnets: [privateSubnets[i % privateSubnets.length].id],
        securityGroups: [natsSg.id],
        assignPublicIp: false,
      },
      serviceRegistries: {
        registryArn: natsDiscoveryServices[i].arn,
      },
      tags,
    },
    { dependsOn: mountTargets },
  );
});

// Maps an ACM cert's DNS validation option to the CNAME you add in Cloudflare.
function certValidationRecord(cert: aws.acm.Certificate) {
  return cert.domainValidationOptions.apply((opts) => ({
    name: opts[0].resourceRecordName,
    type: opts[0].resourceRecordType,
    value: opts[0].resourceRecordValue,
  }));
}

export const apiUrl = apiDomain
  ? `https://${apiDomain}`
  : pulumi.interpolate`http://${alb.dnsName}`;

// --- Cloudflare DNS records to create (see infra/pulumi/README.md) ----------
// `api` → CNAME → apiAlbHostname (DNS only); apex → CNAME → webDistributionDomain.
export const apiAlbHostname = alb.dnsName;
export const webDistributionDomain = webDistribution.domainName;
// Add these CNAMEs to validate the ACM certs (only present when the domain is set).
export const apiCertValidation = apiCert ? certValidationRecord(apiCert) : undefined;
export const webCertValidation = webCert ? certValidationRecord(webCert) : undefined;

export const webBucketName = webBucket.bucket;
export const webDistributionId = webDistribution.id;
export const webUrl = useWebCustomCert
  ? `https://${webDomain}`
  : pulumi.interpolate`https://${webDistribution.domainName}`;
export const billingEnabled = enableBilling;
export const artifactBucket = bucket.bucket;
export const appRepositoryUrl = appRepo.repositoryUrl;
export const ecsClusterArn = cluster.arn;
export const natsUrl = natsURL;
export const apiServiceArn = apiService.id;
export const workerServiceArns = workerServices.map((service) => service.id);
export const natsServiceArns = natsServices.map((service) => service.id);
