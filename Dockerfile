# Build the API as a static binary and ship it on distroless.
#
# The runtime image contains the binary, CA certificates and /etc/passwd. No
# shell, no package manager, no build toolchain — which removes an entire class
# of problem rather than mitigating it.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 makes the binary static, which is what allows distroless.
# -trimpath keeps build paths out of the binary; -s -w drops the symbol table
# and DWARF, roughly halving the size.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/storeapi ./cmd/storeapi

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/verify ./cmd/verify

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/seed ./cmd/seed

# static-debian12, not scratch: the binary needs CA roots to reach Cloudflare R2
# and a passwd entry to run as a non-root user. Timezone data is embedded in the
# binary via `import _ "time/tzdata"`, so no tzdata layer is needed.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/storeapi /usr/local/bin/storeapi
COPY --from=build /out/verify   /usr/local/bin/verify
COPY --from=build /out/seed     /usr/local/bin/seed

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/storeapi"]
