ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root

RUN mkdir -p /home/scion/.gemini/antigravity-cli \
  && mkdir -p /home/scion/.agents \
  && chown -R scion:scion /home/scion/.gemini /home/scion/.agents

# Download location based on the script at https://storage.googleapis.com/antigravity-public/antigravity-cli/tools/install.sh
RUN curl -fsSL -o cli.tar.gz https://storage.googleapis.com/antigravity-public/antigravity-cli/1.0.0-5288553236791296/linux-x64/cli_linux_x64.tar.gz \
  && tar -xzf cli.tar.gz \
  && mv antigravity /usr/local/bin/agy \
  && chmod +x /usr/local/bin/agy \
  && rm cli.tar.gz

CMD ["agy"]
