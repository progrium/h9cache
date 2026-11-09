import { Container, getContainer } from "@cloudflare/containers";

interface Env {
  H9CACHE: DurableObjectNamespace<H9cache>;
  R2_BUCKET: string;
  R2_ACCOUNT_ID: string;
  R2_ACCESS_KEY_ID: string;
  R2_SECRET_ACCESS_KEY: string;
  H9CACHE_TOKENS?: string;
  H9CACHE_FLAGS?: string;
  H9CACHE_STORE?: string;
}

// One h9cache instance. It sleeps when idle; the images stay in R2.
export class H9cache extends Container<Env> {
  defaultPort = 8080;
  sleepAfter = "30m";

  constructor(ctx: DurableObjectState<{}>, env: Env) {
    super(ctx, env, {
      envVars: {
        R2_BUCKET: env.R2_BUCKET,
        R2_ACCOUNT_ID: env.R2_ACCOUNT_ID,
        R2_ACCESS_KEY_ID: env.R2_ACCESS_KEY_ID,
        R2_SECRET_ACCESS_KEY: env.R2_SECRET_ACCESS_KEY,
        H9CACHE_TOKENS: env.H9CACHE_TOKENS ?? "",
        H9CACHE_FLAGS: env.H9CACHE_FLAGS ?? "",
        H9CACHE_STORE: env.H9CACHE_STORE ?? "",
      },
    });
  }
}

// Every request, WebSocket upgrades included, goes to the container.
export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    return getContainer(env.H9CACHE).fetch(request);
  },
};
