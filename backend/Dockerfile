FROM golang:1.22-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Debug + force module mode (safe to keep)
RUN echo "GOMOD=$(go env GOMOD)" && ls -la && ls -la cmd && ls -la cmd/server

# Force modules mode and build the correct package
RUN CGO_ENABLED=0 GOOS=linux GO111MODULE=on go build -o /out/server ./cmd/server

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/server /app/server
COPY migrations /app/migrations
ENV PORT=8080
EXPOSE 8080
CMD ["/app/server"]
