FROM golang:1.25-alpine
WORKDIR /app
COPY go.mod ./
COPY contracts ./contracts
COPY fixtures ./fixtures
COPY cmd ./cmd
RUN go mod download
RUN go build -o /service ./cmd/server
CMD ["/service"]
