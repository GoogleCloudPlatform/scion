# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build stage
FROM node:20-slim AS build

# Install dependencies for building, including D2 for diagrams
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    ca-certificates \
    make \
    && curl -fsSL https://d2lang.com/install.sh | sh -s -- \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy package files and install dependencies
COPY package*.json ./
RUN npm install

# Copy the rest of the source code
COPY . .

# Build the site
RUN npm run build

# Runtime stage - use unprivileged nginx for security
FROM nginxinc/nginx-unprivileged:stable-alpine

# Copy the built site from the build stage
COPY --from=build /app/dist /usr/share/nginx/html

# Nginx unprivileged image listens on 8080 by default
EXPOSE 8080

CMD ["nginx", "-g", "daemon off;"]