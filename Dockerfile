# One image, two binaries: the controller (gpuremediate) and the lab simulator (gpu-sim).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpuremediate ./cmd/gpuremediate \
 && go build -trimpath -ldflags "-s -w" -o /out/gpu-sim ./cmd/gpu-sim

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gpuremediate"]
