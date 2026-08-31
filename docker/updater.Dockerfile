FROM docker:27-cli

RUN apk add --no-cache ca-certificates docker-cli-compose git
COPY docker/updater.sh /usr/local/bin/ecs-controller-updater
RUN chmod 0755 /usr/local/bin/ecs-controller-updater

ENTRYPOINT ["/usr/local/bin/ecs-controller-updater"]
