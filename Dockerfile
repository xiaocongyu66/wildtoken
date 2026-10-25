# ── Console build stage ──────────────────────────────────────────────────────
# The console is served at /console. It builds here so
# `docker compose up -d --build` stays the single deployment command; the
# shared CSS, fonts and theme packs under static/ are copied as-is.
FROM node:24-bookworm-slim AS console-builder

WORKDIR /web

# Lockfile first: dependency installs are cached until the manifest changes.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

# ── Rust build stage ─────────────────────────────────────────────────────────
FROM rust:bookworm AS builder

WORKDIR /src

COPY Cargo.toml Cargo.lock ./
COPY src ./src

RUN cargo build --locked --release --bin wildtoken

# ── Runtime stage ────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        locales \
        tzdata \
    && sed -i 's/^# \(en_US.UTF-8 UTF-8\)/\1/' /etc/locale.gen \
    && locale-gen \
    && ln -snf /usr/share/zoneinfo/Asia/Singapore /etc/localtime \
    && echo 'Asia/Singapore' > /etc/timezone \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /src/target/release/wildtoken /usr/local/bin/wildtoken
COPY static ./static
COPY config ./config

# The React console's build output. Kept out of the image when the stage is
# skipped would break /console, so it is copied unconditionally.
COPY --from=console-builder /web/dist ./web/dist

# Themes are runtime-only, so copying them last keeps theme edits from
# invalidating the compile cache.
COPY themes ./themes

ENV APP__SERVER__HOST=0.0.0.0 \
    APP__SERVER__PORT=3100 \
    DATABASE_URL=sqlite:/data/wildtoken.db?mode=rwc \
    WILDTOKEN_IMAGE_DIR=/data/images \
    WILDTOKEN_LOG=info \
    LANG=en_US.UTF-8 \
    LANGUAGE=en_US:en \
    LC_ALL=en_US.UTF-8 \
    TZ=Asia/Singapore

VOLUME ["/data"]
EXPOSE 3100

CMD ["wildtoken"]
