FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
WORKDIR /src

# Get target architecture for cross-compilation
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build the specific binary based on the build argument and target architecture
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} make build

# STEP 2: Build small image
FROM gcr.io/distroless/static-debian12
COPY --from=builder /src/bin/kube-ingress /bin/kube-ingress

CMD ["/bin/kube-ingress"]