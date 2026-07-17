ARG VERSION=0.0.0
FROM --platform=$BUILDPLATFORM node:lts AS uibuilder
RUN rm -rf /var/lib/dpkg/tmp.ci /var/lib/dpkg/updates /var/cache/debconf
ENV PNPM_HOME="/pnpm"
ENV PATH="$PNPM_HOME:$PATH"
RUN corepack enable pnpm && corepack install -g pnpm@latest-9 && rm -rf /var/lib/dpkg/tmp.ci /var/lib/dpkg/updates /var/cache/debconf

WORKDIR /src
#COPY ui/package.json ui/pnpm-lock.yaml /src
#RUN pnpm fetch 

COPY ui .
RUN pnpm install --ignore-scripts && pnpm build

FROM golang:bookworm AS gobuilder
ARG VERSION
WORKDIR /src
COPY . .
COPY --from=uibuilder /src/dist ./ui/dist
#RUN apk add git
RUN go generate ./... && CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" -o rmfakecloud-docker ./cmd/rmfakecloud/

FROM scratch
EXPOSE 3000
ADD ./docker/rootfs.tar /
COPY --from=gobuilder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=gobuilder /src/rmfakecloud-docker /
ENTRYPOINT ["/rmfakecloud-docker"]
