# Firebase setup for push notifications (owner)

Push notifications use Firebase Cloud Messaging (FCM). The backend sends them (customer-api, #252). Each app gets a device token from Firebase and registers it with our API. Never paste a key or the service account JSON in chat, an Issue, the Shared Doc or the repo.

## The one rule
**All three apps and the backend must use the same Firebase project.** A token from one project cannot be sent to with another project's key: FCM refuses it ("SenderId mismatch"). If the rider team has already made a Firebase project for the rider app, use that project. Ask them for its project ID, and add the customer and partner apps to it. Don't create a second project. Use one project for staging now and a separate one for production later.

## Steps

| # | Where | What |
|---|---|---|
| 1 | [console.firebase.google.com](https://console.firebase.google.com) | Sign in with the Ninzo Google account. Either open the rider team's project, or choose **Create a project** (for example `ninzo-staging`). Google Analytics is optional; it can be off. |
| 2 | Project settings, **Cloud Messaging** tab | Check that **Firebase Cloud Messaging API (V1)** says *Enabled*. If it doesn't, use the link there to enable it in Google Cloud. The legacy API is not needed. |
| 3 | Project overview, **Add app**, Android | Once per app (customer, rider, partner): enter the app's **package name** (the developers give it, for example `com.ninzo.customer`), then download **google-services.json**. SHA-1 is not needed for push. |
| 4 | Project overview, **Add app**, iOS | Once per app: enter the **bundle ID**, then download **GoogleService-Info.plist**. |
| 5 | [developer.apple.com](https://developer.apple.com/account) (needs the paid Apple Developer Program), Certificates, Identifiers & Profiles, **Keys**, + | Tick **Apple Push Notifications service (APNs)** and download the **.p8** file. Note its Key ID and your Team ID. It can be downloaded only once, and one key serves every app. |
| 6 | Firebase, Project settings, Cloud Messaging, **Apple app configuration** | For each iOS app, upload the .p8 key under *APNs Authentication Key*, with the Key ID and Team ID. |
| 7 | Firebase, Project settings, **Service accounts** | **Generate new private key**. This downloads a JSON file. It is a secret. |
| 8 | Railway, **customer-api**, Variables | Add `FCM_SERVICE_ACCOUNT_JSON` and paste the **whole content** of that JSON file. Only customer-api sends pushes. The API logs `push: FCM` with the project id when it starts; a broken value stops it from starting. |
| 9 | You, to each app team directly | Send each team its own config files (google-services.json, GoogleService-Info.plist). They go inside the app, so they are not secrets like the service account, but send them to the team directly rather than through the Shared Doc. |

Then tell me, and I'll do a live test to a developer's test phone.

## What each app team does

| App | Backend side | App team |
|---|---|---|
| Rider | Ready. `POST /riders/me/devices` and `DELETE /riders/me/devices/{id}` | Add Firebase Messaging with the config file from **this** project. Ask for notification permission (Android 13+ and iOS). After sign-in, and on every token refresh, register the token with `platform` `android` or `ios`; remove it on sign-out. On a push, read `data.kind` (`offer`, `relocation`, `restock`, `shift`) and the ids, then open the screen and fetch the details from the API. |
| Customer | Ready. `POST /me/devices` and `DELETE /me/devices/{id}` | The same. Read `data.stage` (`payment_failed`, `no_scooter`, `rider_accepted`, `timer_running`, `arrived`, `handed_over`, `ended`, `available`, `ticket_reply`, `refund_processed`), plus `kind` `rider_waiting`, and `order_id`. The notification settings in the app already exist. |
| Partner (hub staff) | **Not ready.** There is no device endpoint or hub push yet (#253, console session) | Add Firebase once the endpoint is in the partner contract. |
| Console | None | Web app: alerts show in the console. |

Every push has a short title and body, which name nobody, plus a `data` block with `topic`, `stage` or `kind`, and ids. Rider offers, and the customer stages `timer_running`, `arrived` and `rider_waiting`, come at high priority and expire after 1 or 5 minutes. A newer push about the same order replaces the one shown. On iOS they show as time-sensitive only if the app turns on the **Time Sensitive Notifications** capability in Xcode (rider and customer apps should).
