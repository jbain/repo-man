# syntax=docker/dockerfile:1

# repoman drives the real `git` and `gh` binaries rather than reimplementing
# them, so the runtime image is not `scratch`: it needs a working git, an ssh
# client for ssh remotes, and the gh CLI for provider listings.

# ---- build ----------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Copy the module files first so dependency resolution is cached independently
# of the source. There are no third-party dependencies today, but this keeps
# the layer ordering correct if that ever changes.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off so the binary runs on any alpine base without a libc mismatch.
# The trimpath + ldflags combination keeps build paths out of the binary and
# makes the build reproducible for a given commit.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/repoman ./cmd/repoman

# ---- test (a separate target so CI can run it without a second build) ------
FROM build AS test
RUN apk add --no-cache git && \
    git config --global user.email ci@example.com && \
    git config --global user.name CI && \
    go vet ./... && go test ./...

# ---- runtime --------------------------------------------------------------
FROM alpine:3.21

# github-cli lives in the community repository, which is enabled by default in
# the official alpine images.
RUN apk add --no-cache \
        ca-certificates \
        git \
        github-cli \
        openssh-client \
        tzdata

# The service reads and writes the user's checkout tree on a bind mount, so it
# must run as a uid that owns those files. Override at build time to match the
# host user: --build-arg UID=$(id -u) --build-arg GID=$(id -g)
#ARG UID=1000
#ARG GID=1000
# No `|| true` here: if the uid or gid collides with an account the base image
# already has, the build should fail loudly rather than produce an image that
# runs as the wrong user and writes unreadable clones.
#RUN addgroup -g "${GID}" repoman && \
#    adduser -D -u "${UID}" -G repoman -h /home/repoman repoman

RUN mkdir /git #&& chown repoman:repoman /git
COPY --from=build /out/repoman /usr/local/bin/repoman

#USER ${UID}:${GID}
#ENV HOME=/home/repoman \
ENV REPOMAN_ROOT=/git \
    REPOMAN_ADDR=0.0.0.0:8090 \
    GH_NO_UPDATE_NOTIFIER=1 \
    GIT_TERMINAL_PROMPT=0

# The checkout tree is a bind mount; declaring it makes the contract obvious
# and stops a forgotten -v from silently writing into the container layer.
VOLUME ["/git"]
EXPOSE 8090

# No shell form: repoman must receive SIGTERM directly so its graceful
# shutdown runs instead of being killed by the shell's PID 1.
ENTRYPOINT ["/usr/local/bin/repoman"]
