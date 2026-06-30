# ============================================================================
# Go builder — the wish SSH server and the authlaunch OIDC SSO gate
# ============================================================================
# go 1.25: required by go-oidc v3 / x/oauth2 (see go.mod).
FROM docker.io/golang:1.25 AS go-builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -o /wish-server . \
 && CGO_ENABLED=0 go build -o /authlaunch ./cmd/authlaunch


# ============================================================================
# C builder — compiles Brogue CE (g10s fork) as a terminal-only ncurses build
# ============================================================================
FROM docker.io/debian:bookworm-slim AS c-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libncurses-dev pkg-config \
    ca-certificates git \
 && rm -rf /var/lib/apt/lists/*

# Our fork of Brogue CE, pinned to an immutable tag. The fork adds a SIGHUP
# hangup-save so an SSH disconnect cleanly saves the in-progress game, and fixes
# an upstream short-overflow in _delayUpTo that hung the terminal title screen
# for up to ~32s on startup (see geechorama/BrogueCE @ v1.15.1-g10s3).
# TERMINAL=YES GRAPHICS=NO yields a pure-ncurses binary (no SDL) at bin/brogue.
WORKDIR /tmp
RUN git clone --depth 1 --branch v1.15.1-g10s3 \
      https://github.com/geechorama/BrogueCE.git brogue
WORKDIR /tmp/brogue
RUN make TERMINAL=YES GRAPHICS=NO
# Capture the immutable game bits at /opt/brogue (binary + resources). Brogue is
# launched with --data-dir /opt/brogue and writes per-user saves to the player's
# cwd on the data volume, so nothing here is mutated at runtime. chmod a+rX so the
# shed-to games user can read resources and exec the binary.
RUN mkdir -p /opt/brogue \
 && cp -a bin/. /opt/brogue/ \
 && chmod -R a+rX /opt/brogue


# ============================================================================
# Runtime image
# ============================================================================
FROM docker.io/debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libncurses6 libncursesw6 ncurses-term \
    util-linux \
    tzdata \
    ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /wish-server /usr/local/bin/wish-server
# authlaunch — the OIDC SSO gate, exec'd by brogue-launch.sh between the
# wish server and Brogue.
COPY --from=go-builder /authlaunch /usr/local/bin/authlaunch
COPY --from=c-builder /opt/brogue /opt/brogue

COPY brogue-launch.sh /usr/local/bin/brogue-launch.sh
COPY brogue-run.sh /usr/local/bin/brogue-run.sh
RUN chmod +x /usr/local/bin/brogue-launch.sh /usr/local/bin/brogue-run.sh

EXPOSE 2222 2223
ENV WISH_COMMAND=/usr/local/bin/brogue-launch.sh
ENTRYPOINT ["/usr/local/bin/wish-server"]
