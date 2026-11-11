# The tests run in the build: a red test fails the image build instead of shipping.
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY . .
RUN go vet ./... && go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /regulator .

FROM scratch
COPY --from=build /regulator /regulator
ENTRYPOINT ["/regulator"]
