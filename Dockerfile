FROM golang:latest

RUN curl https://glide.sh/get | sh

RUN mkdir -p /go/src/lib
WORKDIR /go/src/lib

COPY glide.yaml /go/src/lib/
COPY glide.lock /go/src/lib/

RUN glide install

COPY . /go/src/lib
