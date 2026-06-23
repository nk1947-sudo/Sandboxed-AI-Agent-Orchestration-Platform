# web/Dockerfile — build the React dashboard and serve it via nginx.
#
# The control plane runs on the HOST (it needs /dev/kvm, the setuid jailer, host
# cgroups and /srv chroots, so it cannot be containerized). This image therefore
# only serves the static dashboard and reverse-proxies /api and /terminal to the
# host control plane. Run it with host networking so 127.0.0.1:7777 reaches the
# control plane:
#
#   docker build -t sandbox-ui ./web
#   docker run -d --name sandbox-ui --network host sandbox-ui
#   # open http://localhost:3000
#
# Stop the Vite dev server first if it is holding port 3000.

# ── build stage ──────────────────────────────────────────────────────────────
FROM node:20-alpine AS build
WORKDIR /app

# Install deps against the committed lockfile for reproducibility.
COPY package.json package-lock.json ./
RUN npm ci

# Build the static bundle (tsc typecheck + vite build → /app/dist).
COPY . .
RUN npm run build

# ── serve stage ──────────────────────────────────────────────────────────────
FROM nginx:alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 3000
CMD ["nginx", "-g", "daemon off;"]
