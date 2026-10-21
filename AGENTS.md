# Switch implementation scope

Work only within `/Users/proletarius/Desktop/Switch/`. The user's copied
`Proletarius`, `Boo`, and `9.29` directories in that root are reference seeds;
the editable application source is `src`. Do not access the original folders
outside Switch. Keep build output and test runtime state inside Switch.

Follow `../docs/Switch改造方案.md` V3 and its execution update. Remove fax,
preserve all other existing behavior, and add independent multi-profile
VoWiFi SMS with a Möbius sidebar icon. New sessions are SMS-only; existing
voice functionality remains. Do not run real card mutations or send SMS
without a clearly selected test card and destination from the user.

Do not generate any DMG unless the user explicitly requests it. Build the
application and test binaries only. `SWITCH_DATA_DIR` should point inside
`../build/runtime` during development. Do not run broad pkill commands or
read/migrate the original application's user data.
