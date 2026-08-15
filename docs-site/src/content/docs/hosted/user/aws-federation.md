---
title: "Cross-Cloud Identity: AWS Access via GCP Federation"
description: Configure Scion agents with GCP service account identities to securely access AWS resources using Workload Identity Federation.
---

When running Scion agents in a hosted environment (such as Google Kubernetes Engine or Cloud Run), your agents are assigned a Google Cloud Platform (GCP) service account identity. Rather than generating and managing static, long-lived AWS Access Keys (`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`) to access AWS resources like Amazon S3, Amazon Bedrock, or DynamoDB, you can use **GCP-to-AWS Workload Identity Federation**.

This guide explains how to configure your AWS IAM roles and Scion agent environments to exchange a GCP identity token for short-lived, auto-rotating AWS temporary credentials.

---

## How It Works

Instead of storing secrets, the agent retrieves a Google-signed OpenID Connect (OIDC) identity token from the local GCP metadata server and exchanges it directly with the AWS Security Token Service (STS).

```d2
direction: lr
classes: {
  box: {
    style: {
      fill: "#f8f9fa"
      stroke: "#cbd5e1"
      stroke-width: 1
    }
  }
}

gcp: "Google Cloud Platform" {
  metadata: "GCP Metadata Server" {
    style.fill: "#eff6ff"
    style.stroke: "#3b82f6"
  }
}

agent: "Scion Agent Container" {
  class: box
  helper: "Credential Helper" {
    style.fill: "#f5f3ff"
    style.stroke: "#8b5cf6"
  }
}

aws: "Amazon Web Services" {
  sts: "AWS Security Token Service (STS)" {
    style.fill: "#fff7ed"
    style.stroke: "#f97316"
  }
  resource: "AWS Resource (S3, Bedrock, etc.)" {
    style.fill: "#f0fdf4"
    style.stroke: "#22c55e"
  }
}

gcp.metadata -> agent.helper: "1. Identity Token\n(aud: sts.amazonaws.com)"
agent.helper -> aws.sts: "2. AssumeRoleWithWebIdentity"
aws.sts -> agent.helper: "3. Temporary Credentials\n(AWS Session)"
agent -> aws.resource: "4. Direct SDK / CLI Access"
```

1. **Token Minting**: The agent fetches a GCP identity token from the metadata server with the audience set to `sts.amazonaws.com`.
2. **STS Exchange**: The agent sends the token to AWS STS via the `AssumeRoleWithWebIdentity` API.
3. **Session Issuance**: AWS STS validates the Google signature, evaluates the IAM role trust policy, and returns temporary AWS session credentials (valid for 1 hour by default).
4. **AWS Access**: The agent uses these temporary credentials to securely authenticate with AWS services.

---

## Prerequisites

Before starting, ensure you have:

1. **A GCP Service Account**: Assigned to your Scion agents. In Hosted mode, this is handled via Scion's Service Account assignment or passthrough. See [Permissions & Policy](/scion/hosted/ha/permissions/) for setup details.
2. **Agent Container Tools**: The `gcloud` and `aws` CLIs must be installed and available inside your agent's container image.

---

## Step 1: AWS IAM Role Setup

You must create an IAM role in AWS that trusts Google's OIDC issuer (`accounts.google.com`) and permits the `sts:AssumeRoleWithWebIdentity` action.

:::danger[Critical AWS OIDC Configuration Rule]
**Do NOT create a custom IAM OIDC identity provider for `accounts.google.com`** in the AWS IAM Console. 

AWS has *built-in* federated support for Google, Facebook, and Amazon. If you create a custom OIDC provider for `accounts.google.com`, it will shadow and break the built-in validation path entirely. AWS STS will reject all valid Google tokens with a generic and unhelpful error:
`InvalidIdentityToken: The web identity token provided could not be validated.`
:::

### Google Condition Key Mappings

When configuring the trust policy, Google's OpenID Connect claims map non-obviously to AWS IAM condition keys. You **must** use the correct keys to prevent access rejection:

| AWS Condition Key | Matches JWT Claim | Expected Value | Purpose |
| :--- | :--- | :--- | :--- |
| `accounts.google.com:oaud` | `aud` (Audience) | `sts.amazonaws.com` | Restricts the token to AWS STS. |
| `accounts.google.com:aud` | `azp` (Authorized Party) | *Numeric GCP Client ID* | **Do not use for audience validation.** STS maps this to Google's numeric client ID, which is the Service Account unique ID. |
| `accounts.google.com:sub` | `sub` (Subject) | *Numeric GCP Service Account ID* | Pins the trust policy to your specific GCP Service Account. |

:::caution[The :aud vs :oaud Gotcha]
Using `accounts.google.com:aud = sts.amazonaws.com` in your trust policy is a common mistake. Because STS maps `:aud` to Google's numeric `azp` claim, the condition will never match, causing AWS to return an `AccessDenied` error. **Always use `:oaud` for validating the `sts.amazonaws.com` audience.**
:::

### Finding Your GCP Service Account Unique ID

To secure the trust policy, you should pin it to your GCP service account's unique numeric ID (not its email address). This prevents any other Google account from assuming your role. 

Retrieve the unique ID using the `gcloud` CLI:

```bash
gcloud iam service-accounts describe YOUR_GCP_SA_EMAIL --format="value(uniqueId)"
```
*(Example output: `117142712507455700323`)*

### Trust Policy Definition

Create your AWS IAM role (e.g., `gcp-scion-federated`) with the following trust policy, replacing the placeholders with your actual GCP Service Account stable numeric unique ID:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "accounts.google.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "accounts.google.com:sub": "YOUR_GCP_SA_UNIQUE_NUMERIC_ID",
          "accounts.google.com:oaud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
```

Attach your required AWS permissions policies (e.g., `AmazonS3ReadOnlyAccess`, `AmazonBedrockFullAccess`) directly to this role.

---

## Step 2: Agent Configuration

Once the AWS role is configured, you can set up your Scion agents to handle the credential exchange. There are three approaches, depending on your workflow.

### Approach A: Credential Process Helper (Recommended)

Using the AWS CLI's `credential_process` configuration is the cleanest and most robust method. It delegates the token fetch and AWS STS exchange to a lightweight script. The AWS CLI and SDKs invoke this helper automatically and handle credential caching and background rotation seamlessly.

#### 1. Save the Helper Script

Save the following script inside your agent container image or inject it via a [Pre-Start Hook](/scion/hosted/user/pre-start-hooks/) at `~/bin/aws-federated-creds.sh`:

```bash
#!/bin/bash
# GCP -> AWS federated credential helper for use as an AWS CLI credential_process.
#
# Exchanges this container's GCP service-account identity token (from the
# metadata server) for temporary AWS credentials via AssumeRoleWithWebIdentity.

set -o pipefail

# AWS CLI v2 pipes output through a pager (less) when attached to a TTY, which
# looks like a hang when this script is run manually. Disable it.
export AWS_PAGER=""

ROLE_ARN="arn:aws:iam::YOUR_AWS_ACCOUNT_ID:role/YOUR_ROLE_NAME"
AUDIENCE="sts.amazonaws.com"
SESSION_NAME="gcp-${HOSTNAME:-cli}-$$"
AWS_BIN="$(command -v aws)"

if [ -z "$AWS_BIN" ]; then
    echo "Error: aws CLI not found." >&2; exit 1
fi

# 1. Fetch a fresh OIDC token from GCP (metadata-server backed, ~1h lifetime).
# Stderr is routed to /dev/null so gcloud warnings don't break the CLI's JSON parser.
GCP_TOKEN=$(gcloud auth print-identity-token --audiences="$AUDIENCE" 2>/dev/null)

if [ -z "$GCP_TOKEN" ]; then
    echo "Error: Failed to obtain GCP identity token. Ensure you are authenticated with gcloud." >&2
    exit 1
fi

# 2. Exchange the token with AWS STS. The inner aws call is fully isolated:
# AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE point at /dev/null so it can
# never resolve a profile whose credential_process is THIS script (infinite
# recursion / hang), and ambient AWS_* creds are cleared so stale/expired
# exports can't interfere. The call itself is unsigned and needs no creds.
# --query emits the credential_process schema directly (Version must be the
# number 1), so no jq dependency is needed.
env -u AWS_PROFILE -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN \
    AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
    "$AWS_BIN" sts assume-role-with-web-identity \
    --role-arn "$ROLE_ARN" \
    --role-session-name "$SESSION_NAME" \
    --web-identity-token "$GCP_TOKEN" \
    --query '{Version: `1`,
              AccessKeyId: Credentials.AccessKeyId,
              SecretAccessKey: Credentials.SecretAccessKey,
              SessionToken: Credentials.SessionToken,
              Expiration: Credentials.Expiration}' \
    --output json
exit $?
```

Make sure the script is executable:

```bash
chmod +x ~/bin/aws-federated-creds.sh
```

#### 2. Configure AWS Profile

Create or update your `~/.aws/config` file in the agent to define a federated profile:

```ini
[profile gcp-federated]
credential_process = /home/gemini/bin/aws-federated-creds.sh
region = us-east-1
```

#### 3. Verify Authenticated Access

Test the integration by querying the active AWS identity:

```bash
aws --profile gcp-federated sts get-caller-identity
```

---

### Approach B: Web Identity Token File

For standard AWS SDKs and container-aware tools that natively support OIDC identity tokens, you can write the GCP token to a file and point standard environment variables to it.

1. **Refresh the Token File**: GCP identity tokens have a default lifetime of **1 hour**. You must periodically refresh this file (e.g., via a background cron or script):
   ```bash
   gcloud auth print-identity-token --audiences="sts.amazonaws.com" > /tmp/gcp-id-token
   ```

2. **Configure Your Profile or Environment**:
   Configure the profile in `~/.aws/config`:
   ```ini
   [profile gcp-federated]
   role_arn = arn:aws:iam::YOUR_AWS_ACCOUNT_ID:role/YOUR_ROLE_NAME
   web_identity_token_file = /tmp/gcp-id-token
   role_session_name = gcp-cli-session
   ```

   Alternatively, export these values as environment variables:
   ```bash
   export AWS_ROLE_ARN="arn:aws:iam::YOUR_AWS_ACCOUNT_ID:role/YOUR_ROLE_NAME"
   export AWS_WEB_IDENTITY_TOKEN_FILE="/tmp/gcp-id-token"
   export AWS_ROLE_SESSION_NAME="gcp-cli-session"
   ```

AWS SDKs will automatically read `/tmp/gcp-id-token` and handle the STS exchange behind the scenes.

---

### Approach C: Manual (One-Off) STS Exchange

For interactive debugging or one-off workflows, you can manually mint the token, run the exchange, and load the temporary credentials into your environment:

```bash
# 1. Mint Google-signed identity token for sts.amazonaws.com
GCP_TOKEN=$(gcloud auth print-identity-token --audiences="sts.amazonaws.com")

# 2. Exchange with AWS STS and export temporary credentials into the environment
eval $(aws sts assume-role-with-web-identity \
  --role-arn "arn:aws:iam::YOUR_AWS_ACCOUNT_ID:role/YOUR_ROLE_NAME" \
  --role-session-name "gcp-cli-session" \
  --web-identity-token "$GCP_TOKEN" \
  --query 'Credentials.[
    join(``, [`export AWS_ACCESS_KEY_ID=`, AccessKeyId]),
    join(``, [`export AWS_SECRET_ACCESS_KEY=`, SecretAccessKey]),
    join(``, [`export AWS_SESSION_TOKEN=`, SessionToken])]' \
  --output text | tr '\t' '\n')
```

---

## Troubleshooting & Diagnostics

If your cross-cloud credentials fail, the specific STS error is the key diagnostic signal:

### Error: `InvalidIdentityToken`

**Symptom**: `InvalidIdentityToken: The web identity token provided could not be validated.`

**Root Cause**: A custom IAM OIDC identity provider exists in your AWS account for `accounts.google.com`. Because Google is a built-in provider, the custom provider Shadows/corrupts the OIDC endpoint registry. STS fails *before* evaluating your role's trust policy.

**Action**: In the AWS IAM Console, navigate to **Identity Providers**, select the provider for `accounts.google.com`, and **delete it**. Do not recreate it.

---

### Error: `AccessDenied`

**Symptom**: `AccessDenied: Not authorized to perform sts:AssumeRoleWithWebIdentity`

**Root Cause**: The Google-signed token is valid, but the IAM role's trust policy rejected the request. This occurs if:
- The trust policy uses `accounts.google.com:aud` for the audience check instead of `accounts.google.com:oaud`.
- The GCP Service Account numeric unique ID in the `accounts.google.com:sub` condition does not match the actual ID.
- The token audience used in `gcloud auth print-identity-token --audiences="..."` was not exactly `sts.amazonaws.com`.

**Action**: Verify that your trust policy matches the [Step 1 Trust Policy Definition](#trust-policy-definition) exactly, specifically checking the `:oaud` vs `:aud` mapping and matching the numeric GCP Service Account ID.

---

## Security Best Practices

- **Token Lifetimes**: GCP identity tokens are issued with a 1-hour expiration. The credential process helper automatically obtains a fresh token on demand, ensuring expired tokens do not block long-running agent workflows.
- **Zero Static Keys**: Never store static credentials (`AWS_ACCESS_KEY_ID`, etc.) inside your agent configurations or Scion Hub Secrets. Federation provides full credential rotation automatically.
- **Principal Hardening**: Always pin the trust policy using the stable, numeric GCP service account ID (`accounts.google.com:sub`) rather than relying on loose wildcards. Unlike email-based names, numeric IDs cannot be spoofed or reused if the GCP service account is deleted and recreated.
