FROM golang:alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY . .
RUN go get -d -v

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /solr-waiter

FROM scratch
COPY --from=builder /solr-waiter /solr-waiter
ENTRYPOINT ["/solr-waiter"]