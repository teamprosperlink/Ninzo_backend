# W9: Map providers

Version: 0.1 · Updated: 2026-11-11 · Issue: #162 · Part of the geo record (`README.md`)

## Goal
Use outside map providers only where they add something we cannot do with OpenStreetMap and OSRM, within their terms, and never store their data for routing (G5).

## Who does what

| Need | Who | Provider | Backend involvement |
|---|---|---|---|
| Showing maps in the apps | App developers | Google Maps SDK (decision 0011 #12) | None |
| Turn-by-turn navigation for the rider | App developers | Hand-off to the Google Maps app, and our route drawn in the app (`w6`) | Our route only |
| A customer placing a pin, searching an address | App developers | Google Places, or Mappls for better Indian addresses | None, unless a proxy is chosen (below) |
| Ops entering gates and nodes by address | Console session | Same as above | Possibly a proxy |
| Drawing service areas and polygons | Ops, through the console | `data_flow.md` says locations and polygons are pre-built and mapped through Google Maps | The console's map screens; the backend stores the polygons |

`owner_setup_checklist.md` (item 6) already asks the owner for a `GOOGLE_MAPS_SERVER_API_KEY`, restricted by API and IP. So a server-side Google role was expected; G22 settles what it is.
| Road times, cell sets, matching | This backend | **OSRM on OpenStreetMap only** | All of it |

## Options for address search

| Option | For | Against |
|---|---|---|
| **The apps call the provider directly**, with keys restricted to the app | No backend work; the provider's terms are simplest to meet | Each app holds a key |
| The backend proxies address search | Keys stay on the server; one place to switch providers; cost control | Backend work; the proxy must follow the provider's caching terms |

**Recommendation:** apps call directly for the pilot. Revisit when cost or switching matters.

## Providers

| Provider | Strength | Cost and terms to check |
|---|---|---|
| Google Maps Platform | The apps' map; Places search | Free monthly usage per SKU since March 2025 (`research.md`, section 10); limits on caching and on using its content with other maps |
| Mappls (MapmyIndia) | Indian addresses and localities | Paid per call; its own terms |
| Ola Maps | Lower-cost Indian maps | Watch only |
| DIGIPIN (India Post) | An open 10-character code for any 4 m square in India | Free and open: can be computed from a pin and stored beside it |

## What we store
- **The pin the customer or ops confirms** is our data, stored on the gate. Where a provider's search suggested it, record the source, and check that provider's terms on keeping geocoded points before release.
- **The rider's stop at handover** (`w3`): used to suggest gate pin fixes to ops, as Zomato and Swiggy reportedly do.
- **DIGIPIN** (optional): computed from the pin, shown to the rider as a shareable code.

## Steps

| # | Step | Done when |
|---|---|---|
| 1 | Owner's answer to G22 | Recorded in the decision log |
| 2 | Tell the app developers which provider does what | Written in the shared Doc |
| 3 | Gate pin fixes from handover stops | Ops see suggested fixes; nothing changes without them |
| 4 | DIGIPIN on gates (optional) | Shown in the rider's order, if the owner wants it |

## Open questions
- G22: does the backend need Google Maps or Mappls at all, or do only the apps use them?
