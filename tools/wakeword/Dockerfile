# Preflight scaffold only: this is not the WP-24 training image.
# Supply a reviewed Python >=3.11 base by digest. No unpinned default or downloads.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ARG BASE_IMAGE
RUN python3 -c 'import os,re; assert re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", os.environ["BASE_IMAGE"]), "Pin BASE_IMAGE by digest"'
WORKDIR /pipeline
COPY lint_licences.py train.py recipe.yaml DATA-LICENCES.md ./
ENV PYTHONDONTWRITEBYTECODE=1
USER 65532:65532
ENTRYPOINT ["python3", "/pipeline/train.py"]
