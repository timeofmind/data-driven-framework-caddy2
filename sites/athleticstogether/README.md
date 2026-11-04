# athleticstogether.com

A public training planner, training log and virtual-events site, built entirely
from `schema/athleticstogether.yaml` by the datadriven Caddy handler.

- **Plan** — training plans made of planned workouts on a calendar (targets for
  distance, time and pace).
- **Log** — activities typed in or uploaded from a watch (FIT/GPX/TCX) or timing
  system (CSV), with laps; each upload is matched to that day's planned workout,
  which is marked completed (or partial when well short of the target).
- **Virtual events** — anyone can organise one (a distance and a date window);
  athletes register, and uploads inside the window that cover the distance post a
  result to the leaderboard automatically (best per athlete).

Everything is private by default except events; plans, workouts and activities can
each be made public.

## Running

See `Caddyfile`. In production nginx terminates TLS for athleticstogether.com and
proxies to Caddy on 127.0.0.1:8095:

```nginx
location / {
    proxy_pass http://127.0.0.1:8095;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 100m;
}
```
