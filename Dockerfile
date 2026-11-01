FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /podmortem ./cmd/podmortem

# Static binary on a minimal, non-root base image.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /podmortem /podmortem
USER nonroot:nonroot
ENTRYPOINT ["/podmortem"]
