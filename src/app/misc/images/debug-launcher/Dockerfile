FROM centos
ENV TERM=linux

ADD bin/debug-launcher /debug-launcher
ADD entry-point.sh /entry-point.sh
RUN chmod +x /debug-launcher
RUN chmod +x /entry-point.sh
ENTRYPOINT ["/entry-point.sh"]

