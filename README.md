# httpnc
HTTP client and server in a single binary for transferring stdin on one host to stdout on another like nc/socat

## Installation
```
go install github.com/ihanick/httpnc/cmd/httpnc@latest
```

## Usage

Generate ssl keys for the server
```
openssl req -x509 -nodes -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365
```
Start the server on the destination:
```
httpnc -l :8443 -token token123 | tar xz
```
Initiate transer on the source side:
```
tar cz mydir | httpnc -connect https://localhost:8443/upload -token token123
```
