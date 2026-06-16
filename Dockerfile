# PlanMyPickle backend — multi-stage build.
# Pinned to Go 1.26 (Railway's Nixpacks may not have it yet, so we use Docker).
# Pure-Go, no cgo deps (data layer is Supabase REST over HTTPS), so CGO stays
# off and we ship a static binary on distroless.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/api /api
# Railway injects $PORT; main.go binds to it. 8080 is just the local default.
EXPOSE 8080
ENTRYPOINT ["/api"]
