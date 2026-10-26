# The dispatcher agent image (design §4.3, §5). Build from dispatcher-agent/:
#   docker build -t clawbridge-dispatcher-agent dispatcher-agent
# Run it only with the hardening flags in README.md, "The container" (agentd sets them).
FROM node:22.20.0-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e AS deps
WORKDIR /app
COPY package.json package-lock.json .npmrc ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund
COPY scripts/third-party-notices.ts scripts/
RUN node scripts/third-party-notices.ts > THIRD-PARTY-NOTICES

FROM node:22.20.0-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e
# The agent needs node and nothing else: no package manager in the final image.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
        /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /opt/yarn-* /usr/local/bin/yarn /usr/local/bin/yarnpkg
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY --from=deps /app/THIRD-PARTY-NOTICES ./
COPY package.json README.md ./
COPY src ./src
ENV NODE_ENV=production
USER node
ENTRYPOINT ["node", "--max-old-space-size=96", "--disable-warning=ExperimentalWarning", "src/main.ts"]
