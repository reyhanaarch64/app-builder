FROM golang:1.24 AS backend
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ .
RUN CGO_ENABLED=1 go build -o /server .

FROM eclipse-temurin:17-jre-jammy
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates unzip docker.io && rm -rf /var/lib/apt/lists/*
COPY --from=backend /server /server
COPY frontend/dist /app/frontend/dist
EXPOSE 8866
CMD ["/server"]
