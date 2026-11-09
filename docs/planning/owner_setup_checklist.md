# Owner setup checklist

What the owner must create or supply, so that work does not stop. Updated: 2026-11-04.

## How to hand things over
- **Safe to send in chat:** repo URLs, account names, project IDs, region names, and public keys.
- **Never send in chat:** passwords, API secrets, private SSH keys and service account files. Put them in the local `.env` (never committed, and Claude is blocked from reading it). For hosted environments, put them in GitHub or hosting secret stores.
- Create test or staging keys first. Live keys come only at go-live.

## 1. Needed for v0.1.0

| # | Item | What to do | Unblocks |
|---|---|---|---|
| 1 | GitHub repo and login | Finished on 2026-11-04 | Issues, PRs |
| 2 | Railway account | Finished on 2026-11-04: signed in as PROSPER LINK | A stable mock URL |
| 3 | Google Docs connector | Finished on 2026-11-04 | Keeping the index updated |
| 4 | Railway plan | Owner, 2026-11-06: stay on the free plan until full development is done. The free plan gives $1 of credit a month, so the mock or staging may stop if it runs out. Upgrade only with the owner's okay | Hosting |
| 4a | Share the index | Send the developers the [shared index](https://docs.google.com/document/d/1p5sGHeYSo4cCCLlAErqLLqEMnOPkZvk2-gEA4fKgO6M/edit) when the mock is ready, and say when their feedback is in | Contract review |

## 2. Needed for v0.2.0 and v0.3.0

| # | Account | Create | Put in |
|---|---|---|---|
| 4a | Docker Desktop | Start it. It is installed but was not running | Local Postgres and Redis |
| 5 | Supabase | Connected. `Ninzo-backend-mumbai` (`chyejfgsipcsdxnginei`) is live, on the free plan. Its session pooler string is in `.env`. The existing project belongs to a different app and stays untouched | `DATABASE_URL`: session pooler string |
| 6 | Google Maps Platform | A billing-enabled project and a server key restricted by API and IP. Separate restricted keys go to each app | `GOOGLE_MAPS_SERVER_API_KEY` |
| 7 | Decision | Pilot city, for map data and polygons | Reply in chat or an Issue |

## 3. Needed for v0.4.0 to v0.6.0

| # | Account | Create | Put in |
|---|---|---|---|
| 8 | Razorpay | Test mode account and keys. Payment is charged up front, so holds are not needed | `RAZORPAY_KEY_ID`, `RAZORPAY_KEY_SECRET`, `RAZORPAY_WEBHOOK_SECRET` |
| 9 | Firebase | A project with Cloud Messaging, and a service account file kept outside the repo | `FCM_PROJECT_ID`, `FCM_SERVICE_ACCOUNT_FILE` |
| 10 | SMS gateway | In progress on the owner's side (2026-11-09). An account with an Indian provider (for example MSG91), and DLT registration, which takes time. Steps: `sms_setup.md` | `SMS_*` |
| 11 | Telephony | A masked-calling provider (for example Exotel). Can wait, because masked calls are P2 for now | `TELEPHONY_*` |

## 4. Needed for staging and deployment

| # | Account | Create | Put in |
|---|---|---|---|
| 12 | Staging hosting, needed by v0.2.0 | Proposed: AWS, Mumbai region. Please confirm first (status open question 1) | Access set up together, not in chat |
| 13 | Redis | Comes with the hosting choice | `REDIS_URL` |
| 14 | Domain | A domain with DNS access, for example for `api.` and `staging-api.` | DNS records |
| 15 | Sentry | One organisation and one project per service | `SENTRY_DSN` |

## 5. Needed before real customers

| # | Item | Notes |
|---|---|---|
| 16 | Razorpay live mode | Business verification and live keys |
| 17 | FSSAI | Food labelling and expiry rules affect lot and label data |
| 18 | DigiLocker | Rider verification |
| 19 | Supabase Pro or RDS | Free projects pause when idle. Decide on failover (decision 0004) |
| 20 | Chatwoot and Metabase | Named in the ops console doc, India-region hosting |

## 6. For the app teams, not this repo
Apple Developer account, Google Play Console and the Flutter repos belong to the other developers building the apps. Claude does not work on them.
