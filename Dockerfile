# The tests run in the build: a red test fails the image build instead of shipping.
# Built on the build platform and cross-compiled, so a multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN go vet ./... && go test ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /regulator .

FROM scratch
COPY --from=build /regulator /regulator
ENTRYPOINT ["/regulator"]
