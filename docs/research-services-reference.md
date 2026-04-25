# Services Reference: Firecrawl MCP, Apify, Coresignal

Compiled 2026-04-01. This document serves as a technical reference for two downstream agents:
- **Agent A**: Deploying Firecrawl MCP Server on NixOS
- **Agent B**: Building a custom MCP server wrapping Apify + Coresignal APIs

---

## 1. Firecrawl MCP Server

### Overview
Firecrawl provides an MCP (Model Context Protocol) server for web scraping, crawling, search, and content extraction. It can run locally via npx/npm or connect to a remote hosted endpoint.

### GitHub
https://github.com/firecrawl/firecrawl-mcp-server

### Installation Methods

**Remote Hosted (no local install needed):**
```
https://mcp.firecrawl.dev/{FIRECRAWL_API_KEY}/v2/mcp
```

**NPX (quickest local):**
```bash
env FIRECRAWL_API_KEY=fc-YOUR_API_KEY npx -y firecrawl-mcp
```

**Global npm install:**
```bash
npm install -g firecrawl-mcp
```

**Smithery (legacy, Claude Desktop):**
```bash
npx -y @smithery/cli install @mendableai/mcp-server-firecrawl --client claude
```

### MCP JSON Configuration (for Claude Desktop, Cursor, VS Code, etc.)
```json
{
  "mcpServers": {
    "firecrawl-mcp": {
      "command": "npx",
      "args": ["-y", "firecrawl-mcp"],
      "env": {
        "FIRECRAWL_API_KEY": "YOUR-API-KEY"
      }
    }
  }
}
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `FIRECRAWL_API_KEY` | Yes (cloud) | - | API key for Firecrawl cloud |
| `FIRECRAWL_API_URL` | No | Cloud URL | Override for self-hosted instances |
| `FIRECRAWL_RETRY_MAX_ATTEMPTS` | No | 3 | Max retry attempts |
| `FIRECRAWL_RETRY_INITIAL_DELAY` | No | 1000 | Initial retry delay (ms) |
| `FIRECRAWL_RETRY_MAX_DELAY` | No | 10000 | Max retry delay (ms) |
| `FIRECRAWL_RETRY_BACKOFF_FACTOR` | No | 2 | Exponential backoff multiplier |
| `FIRECRAWL_CREDIT_WARNING_THRESHOLD` | No | 1000 | Warn when credits below this |
| `FIRECRAWL_CREDIT_CRITICAL_THRESHOLD` | No | 100 | Critical alert threshold |

### Available MCP Tools

| Tool | Description |
|------|-------------|
| `firecrawl_scrape` | Extract content from a single URL (JSON preferred over markdown) |
| `firecrawl_batch_scrape` | Scrape multiple known URLs efficiently |
| `firecrawl_map` | Discover all indexed URLs on a website/domain |
| `firecrawl_crawl` | Asynchronously crawl entire websites with depth/page limits |
| `firecrawl_check_crawl_status` | Monitor ongoing crawl progress |
| `firecrawl_search` | Web search with optional scraping of results |
| `firecrawl_extract` | LLM-based structured data extraction from pages |
| `firecrawl_agent` | Autonomous research agent for multi-source tasks |
| `firecrawl_agent_status` | Poll agent job completion and retrieve results |
| `firecrawl_interact` | Browser interaction on scraped pages |
| `firecrawl_browser_create` | Create persistent browser sessions via CDP |
| `firecrawl_browser_execute` | Run code (bash/Python/JS) in browser session |
| `firecrawl_browser_delete` | Terminate browser sessions |
| `firecrawl_browser_list` | List active browser sessions |

### Pricing

| Plan | Monthly Cost | Credits | Notes |
|------|-------------|---------|-------|
| Free | $0 | 500 credits | No credit card required |
| Hobby | $16/mo | More credits | - |
| Standard | $83/mo | More credits | - |
| Growth | $333/mo | More credits | - |
| Scale/Enterprise | Custom | Custom | Annual, credits upfront |

**Credit costs:**
- Scraping/crawling: 1 credit per webpage or PDF page
- Stealth Mode (bypass blocks): up to 5 credits per request
- `/extract` with AI schema parsing: up to 5 credits per request
- Credits do NOT roll over (except auto-recharge credits)

### Key Features
- Automatic rate limit handling with exponential backoff
- Streamable HTTP support
- Cloud and self-hosted deployment
- Server-Sent Events (SSE) support
- Comprehensive error handling with automatic retries

### NixOS Deployment Notes
The server is a Node.js package (`firecrawl-mcp` on npm). For NixOS:
- Needs Node.js runtime (18+ recommended)
- Package entry point: `npx -y firecrawl-mcp` or globally installed `firecrawl-mcp`
- Communicates via stdio (standard MCP transport)
- The remote hosted URL (`https://mcp.firecrawl.dev/{KEY}/v2/mcp`) is an alternative that avoids local install entirely
- Environment variables are the only configuration mechanism (no config files)

---

## 2. Apify

### Overview
Apify is a full-stack web scraping and automation platform. It hosts "Actors" (serverless scraping/automation programs) that can be run via API. Relevant for finding niche companies via LinkedIn scraping, Google Maps data, and website content crawling.

### Base URL
```
https://api.apify.com/v2
```

### Authentication
Two methods:
1. **Bearer Token (recommended):** `Authorization: Bearer <token>`
2. **Query Parameter:** `?token=YOUR_TOKEN`

Tokens are found in Apify Console > Integrations page.

### Key API Endpoints

| Purpose | Method | Endpoint |
|---------|--------|----------|
| Run Actor | POST | `/v2/acts/{actorId}/runs` |
| Run Actor (sync, return output) | POST | `/v2/acts/{actorId}/run-sync-get-dataset-items` |
| Get run status | GET | `/v2/acts/{actorId}/runs/{runId}` |
| Get dataset items | GET | `/v2/datasets/{datasetId}/items` |
| Get key-value store | GET | `/v2/key-value-stores/{storeId}/records/{key}` |

**Actor ID format:** `username~actor-name` or numeric ID

### Running an Actor Programmatically

```bash
# Start an actor run
curl -X POST "https://api.apify.com/v2/acts/curious_coder~linkedin-company-scraper/runs?token=YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://www.linkedin.com/company/microsoft"]}'

# Synchronous run (waits up to 5 min, returns dataset items directly)
curl -X POST "https://api.apify.com/v2/acts/curious_coder~linkedin-company-scraper/run-sync-get-dataset-items?token=YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://www.linkedin.com/company/microsoft"]}'
```

**Optional query parameters for runs:**
- `memory=8192` - allocate specific RAM (MB)
- `build=beta` - specify build version
- `waitForFinish=60` - wait up to N seconds (max 60)

**Completion strategies for long runs:**
1. Synchronous endpoint (max 5 minutes)
2. `waitForFinish` parameter (max 60 seconds)
3. Webhooks (POST to your server on completion)
4. Polling the run status endpoint

**Response format:** JSON with `data` wrapper:
```json
{ "data": { "id": "...", "defaultDatasetId": "...", "status": "SUCCEEDED" } }
```

### Rate Limits
- Global: 250,000 requests per minute
- Per-resource default: 60 requests per second
- Higher-tier endpoints: 200-400 requests per second
- Exceeded limits return HTTP 429; use exponential backoff

### SDK / Client Libraries

| Language | Package | Install |
|----------|---------|---------|
| JavaScript/TypeScript | `apify-client` | `npm install apify-client` |
| Python | `apify-client` | `pip install apify-client` |
| Go | None official | Use raw HTTP (standard REST API) |

**JavaScript example:**
```javascript
import { ApifyClient } from 'apify-client';
const client = new ApifyClient({ token: 'YOUR_API_TOKEN' });
const run = await client.actor('curious_coder/linkedin-company-scraper').call({
  urls: ['https://www.linkedin.com/company/microsoft']
});
const { items } = await client.dataset(run.defaultDatasetId).listItems();
```

**Python example:**
```python
from apify_client import ApifyClient
client = ApifyClient(token='YOUR_API_TOKEN')
run = client.actor("curious_coder/linkedin-company-scraper").call(
    run_input={"urls": ["https://www.linkedin.com/company/microsoft"]}
)
for item in client.dataset(run["defaultDatasetId"]).iterate_items():
    print(item)
```

### Pricing

| Plan | Monthly Cost | Credits Included | CU Rate |
|------|-------------|-----------------|---------|
| Free | $0 | $5/month | $0.30/CU |
| Starter | $39/mo | $39 | $0.40/CU |
| Scale | $199/mo | $199 | $0.30/CU |
| Business | $999/mo | $999 | $0.25/CU |

**Compute Unit (CU):** 1 GB of RAM running for 1 hour.
Typical scraping job: $0.001 - $0.05 per run.
Website Content Crawler: ~$0.50 - $5.00 per 1,000 pages.

### Relevant Actors for Niche Company Discovery

#### 1. LinkedIn Company Scraper (`curious_coder/linkedin-company-scraper`)
- **What it does:** Extracts company data from LinkedIn company pages (name, address, phone, website, employee count, industry, specialties)
- **Input:** `urls` array of LinkedIn company URLs, `minDelay`, `maxDelay`
- **URL format:** `https://www.linkedin.com/company/microsoft` or numeric IDs
- **Pricing:** $10.00/month subscription + platform usage
- **API ID:** `curious_coder~linkedin-company-scraper`

#### 2. LinkedIn Sales Navigator Scraper (`curious_coder/linkedin-sales-navigator-search-scraper`)
- **What it does:** Extracts data from Sales Navigator people and company search results (email, social media, website, job titles)
- **Input:** Sales Navigator search parameters/URLs
- **Output:** Lead/prospect data with emails, social profiles, company websites
- **Pricing:** $39.00/month subscription + platform usage
- **Note:** Requires LinkedIn Sales Navigator account; uses random delays for anti-detection
- **API ID:** `curious_coder~linkedin-sales-navigator-search-scraper`

#### 3. Google Maps Scraper (`compass/crawler-google-places`)
- **What it does:** Extracts business data from Google Maps (names, addresses, websites, phone numbers, ratings, reviews, categories, opening hours)
- **Input:** Google Maps URLs OR search term + location combinations
- **Pricing:** Pay-per-event model (~$0.002 per place for detailed data)
- **Tip:** Multiple similar search terms increase results but also runtime
- **API ID:** `compass~crawler-google-places`

#### 4. Website Content Crawler (`apify/website-content-crawler`)
- **What it does:** Crawls websites and extracts text content in plain text, Markdown, or HTML (designed for LLM/RAG pipelines)
- **Input:** `startUrls` (array), `crawlerType` (playwright:adaptive, playwright:firefox, cheerio, jsdom, playwright:chrome)
- **Output:** JSON/CSV with page content in chosen format
- **Pricing:** Free actor; pay only platform usage (~$0.50-$5.00 per 1,000 pages)
- **Note:** Also available as its own MCP server
- **API ID:** `apify~website-content-crawler`

---

## 3. Coresignal

### Overview
Coresignal provides professional network data (companies, employees, jobs) via REST APIs. Data is sourced from professional networks and enriched. Their Company API is the primary interest for enriching company records with firmographic data.

### Base URL
```
https://api.coresignal.com/cdapi/v2/
```

### Authentication
- **Method:** API Key in custom header
- **Header:** `apikey: {API_KEY}`
- **How to get:** Dashboard at `dashboard.coresignal.com` or from account manager
- No OAuth, no bearer tokens -- simple API key header only

```bash
curl -H "apikey: YOUR_API_KEY" \
     -H "accept: application/json" \
     "https://api.coresignal.com/cdapi/v2/company_base/collect/123456"
```

### Key Endpoints

#### Search Endpoint (Simple Filters)
```
POST https://api.coresignal.com/cdapi/v2/company_base/search/filter
```
Request body uses simple key-value filters (see Search Filters below).

#### Elasticsearch DSL Search
```
POST https://api.coresignal.com/cdapi/v2/company_base/search/es_dsl
```
Full Elasticsearch query DSL for complex queries.

#### Collect (by ID)
```
GET https://api.coresignal.com/cdapi/v2/company_base/collect/{company_id}
```

#### Collect (by profile URL / shorthand name)
```
GET https://api.coresignal.com/cdapi/v2/company_base/collect/{profile_url_or_shorthand}
```
Optional query param: `fields=id&fields=name&fields=created` for selective field retrieval.

#### Bulk Collect
```
POST https://api.coresignal.com/cdapi/v2/company_base/bulk_collect
```

### Search Filters (Simple Filter Endpoint)

| Filter | Type | Description | Example |
|--------|------|-------------|---------|
| `name` | String | Company name, supports AND/OR | `"name": "Microsoft"` |
| `website` | String | Company website | `"website": "microsoft.com"` |
| `exact_website` | String | Exact website match | `"exact_website": "https://www.microsoft.com"` |
| `industry` | String | Industry, supports AND/OR | `"industry": "(Information technology) OR Internet"` |
| `size` | String | Company size bucket | See docs |
| `country` | String | HQ country, supports OR | `"country": "United States"` |
| `location` | String | Geographic, supports AND/OR | `"location": "United States, Florida"` |
| `employees_count_gte` | Integer | Min employee count | `"employees_count_gte": 10` |
| `employees_count_lte` | Integer | Max employee count | `"employees_count_lte": 500` |
| `founded_year_gte` | Integer | Min founding year | `"founded_year_gte": 2015` |
| `founded_year_lte` | Integer | Max founding year | - |
| `funding_total_rounds_count_gte` | Integer | Min funding rounds | `"funding_total_rounds_count_gte": 1` |
| `funding_total_rounds_count_lte` | Integer | Max funding rounds | - |
| `funding_last_round_type` | String | Type of last round | See docs |
| `funding_last_round_date_gte` | String | Last round after date | `"funding_last_round_date_gte": "2023-01-01"` |
| `funding_last_round_date_lte` | String | Last round before date | - |
| `created_at_gte` | String | Record created after | `"created_at_gte": "2024-01-01 00:00:00"` |
| `created_at_lte` | String | Record created before | - |
| `last_updated_gte` | String | Record updated after | - |
| `last_updated_lte` | String | Record updated before | - |
| `deleted` | Boolean | Record availability | `true` or `false` |
| `source_id` | Integer | Source identifier | Numeric |

### Elasticsearch DSL Schema (Key Fields)

| Field | Type | Notes |
|-------|------|-------|
| `name` | text (+ exact keyword) | Company name |
| `industry` | text (+ exact keyword) | Industry classification |
| `website` | text (+ exact/filter) | Company URL |
| `size` | keyword | Size bucket |
| `type` | text | Company type |
| `employees_count` | long | -1 if unknown |
| `followers` | long | Social followers |
| `founded` | long | Year |
| `headquarters_city` | keyword | HQ city |
| `headquarters_state` | keyword | HQ state |
| `headquarters_country` | keyword | HQ country |
| `description` | text | Company description |
| `url` | text (+ exact) | Profile URL |
| `canonical_url` | text (+ exact) | Canonical URL |
| `created` | date | Record creation |
| `last_updated` | date | Last update |
| `deleted` | byte | Deletion flag |

**Example ES DSL query (companies in tech with 10-500 employees):**
```json
{
  "query": {
    "bool": {
      "must": [
        { "match": { "industry": "Information Technology" } }
      ],
      "filter": [
        { "range": { "employees_count": { "gte": 10, "lte": 500 } } }
      ]
    }
  },
  "sort": ["_score"]
}
```

### Rate Limits

| Endpoint Type | Rate |
|--------------|------|
| Search (POST) | 18 requests/second |
| Collect (GET) | 54 requests/second |
| Bulk Collect (POST/GET) | 27 requests/second |
| Enrich (GET, Company only) | 18 requests/second |

### Credit System

| API | Search Credits | Collect Credits |
|-----|---------------|----------------|
| Base Company API | 1 per query | 1 per record |
| Clean Company API | 1 per query | 1 per record |
| Multi-source Company API | 2 per query | 2 per record |
| Base Employee API | 1 per query | 1 per record |
| Multi-source Employee API | 2 per query | 2 per record |

- Bulk collect: credits = number of records downloaded
- Remaining credits visible in `x-credits-remaining` response header
- Insufficient credits returns HTTP 402

### Pricing

| Plan | Monthly Cost | Notes |
|------|-------------|-------|
| Free Trial | $0 | 400 search + 200 collect credits, valid 7 days, no card |
| Starter | $49/mo | Monthly renewable credits |
| Higher tiers | Custom | Contact sales |
| Datasets | From $1,000 | Full data dumps |
| Enterprise | Custom | Custom credit allocation |

**Per the user's info:** enrichment costs approximately $0.10-$0.15 per record.

### SDK / Client Libraries
- **No official SDK.** Coresignal is a pure REST API.
- Use any HTTP client (curl, Go net/http, Python requests, etc.)
- Response format: `application/json`

### Response Monitoring
- `x-credits-remaining` header on every response
- HTTP 402 when credits exhausted
- HTTP 429 when rate limited

---

## Cross-Service Architecture Notes

### For Agent A (NixOS Firecrawl MCP Deployment)
- The Firecrawl MCP server is a Node.js package (`firecrawl-mcp`)
- It communicates via stdio (standard MCP protocol)
- Only configuration is via environment variables -- no config files to manage
- Alternative: use the remote hosted MCP URL to avoid running Node.js locally
- For NixOS: need `nodejs` in the system/user environment, then either `npx -y firecrawl-mcp` or a Nix derivation wrapping the npm package
- The `FIRECRAWL_API_KEY` env var must be set at runtime

### For Agent B (Custom MCP wrapping Apify + Coresignal)
- Both Apify and Coresignal are standard REST APIs -- no special SDKs required for Go
- Apify: Bearer token auth, JSON request/response, async runs with polling or sync endpoints
- Coresignal: Custom `apikey` header, JSON request/response, synchronous endpoints
- Recommended workflow for company discovery:
  1. Use Coresignal ES DSL search to find companies by industry/size/location
  2. Use Apify LinkedIn Company Scraper to enrich with LinkedIn data
  3. Use Apify Google Maps Scraper for local business data
  4. Use Apify Website Content Crawler to extract company website content
  5. Use Firecrawl (via MCP or API) for additional web scraping needs
- Go HTTP client is sufficient for both APIs; no third-party SDK needed
- Both APIs return JSON; use standard `encoding/json` for marshaling

### Rate Limit Summary

| Service | Limit |
|---------|-------|
| Firecrawl | Automatic retry with backoff (plan-dependent) |
| Apify | 250K req/min global, 60 req/sec per resource |
| Coresignal Search | 18 req/sec |
| Coresignal Collect | 54 req/sec |
