FROM golang:1.13.7-alpine3.11 AS build
ENV GOPROXY=https://goproxy.cn

RUN mkdir -p /go/src/github.com/zdnscloud/application-operator
COPY . /go/src/github.com/zdnscloud/application-operator

WORKDIR /go/src/github.com/zdnscloud/application-operator
RUN CGO_ENABLED=0 GOOS=linux go build cmd/application-operator/application-operator.go


FROM scratch
LABEL maintainers="Zcloud Authors"
LABEL description="K8S Application CRD Operator"
COPY --from=build /go/src/github.com/zdnscloud/application-operator/application-operator /application-operator
ENTRYPOINT ["/application-operator"]
