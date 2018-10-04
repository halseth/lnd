#!/bin/sh

for file in *.proto **/*.proto
do

    echo "Generating protos from ${file}"

    # Generate the protos.
    protoc -I/usr/local/include -I. \
           -I$GOPATH/src \
           -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
           --go_out=plugins=grpc:. \
           ${file}

    # Generate the REST reverse proxy.
    protoc -I/usr/local/include -I. \
           -I$GOPATH/src \
           -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
           --grpc-gateway_out=logtostderr=true:. \
           ${file}

    # Finally, generate the swagger file which describes the REST API in detail.
    protoc -I/usr/local/include -I. \
           -I$GOPATH/src \
           -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
           --swagger_out=logtostderr=true:. \
           ${file}

done
