// Alpine.js component for yonder.
//
// Single state machine: keep the latest server-side state in `state`,
// refetch after every mutation, surface transient `error`.

function vpnui() {
    return {
        // Server-side snapshot (null until first load)
        state: null,

        // Add-subscription form fields
        newSubLabel: "",
        newSubSource: "",
        showAddForm: false,

        // Rules form (set-URL flow)
        newRulesUrl: "",

        // UI flags
        busy: false,
        error: "",

        // ---- Lifecycle ----

        init() {
            this.refresh();
            // Adaptive polling. When the apply worker is active we want to
            // see state.applying flip back to false as soon as possible
            // (so the UI unfreezes); the rest of the time a relaxed
            // cadence is fine for picking up changes from other clients.
            this._pollTick();
        },

        _pollTick() {
            const delay = (this.state && this.state.applying) ? 1500 : 10000;
            setTimeout(async () => {
                if (!this.busy) await this.refresh(true);
                this._pollTick();
            }, delay);
        },

        // ---- Computed ----

        // True while either the local POST is in flight (`busy`) OR the
        // apply worker on the server is running an xkeen restart. The
        // latter makes the disabled state visible across browser tabs and
        // for clients that didn't initiate the change.
        get busyOrApplying() {
            return this.busy || (this.state && this.state.applying);
        },

        get hasAnyServers() {
            if (!this.state || !this.state.subscriptions) return false;
            return this.state.subscriptions.some(s => s.servers.length > 0);
        },

        get activeServerLabel() {
            const a = this.state && this.state.active_server;
            if (!a) return "";
            for (const sub of this.state.subscriptions || []) {
                if (sub.id !== a.subscription_id) continue;
                const srv = sub.servers.find(s => s.id === a.server_id);
                if (srv) return `${srv.name} (${sub.label})`;
            }
            return `${a.server_id} (${a.subscription_id})`;
        },

        isActive(subId, srvId) {
            const a = this.state && this.state.active_server;
            return a && a.subscription_id === subId && a.server_id === srvId;
        },

        sourcePreview(s) {
            if (!s) return "";
            if (s.startsWith("vless://")) {
                // Inline link — show a short, non-sensitive summary.
                // (Full source is in the daemon's state but no need to splash UUID in the UI.)
                const at = s.indexOf("@");
                const hashOrQuery = Math.min(...[s.indexOf("?"), s.indexOf("#")].filter(i => i > 0).concat([s.length]));
                const hostPart = at > 0 ? s.slice(at + 1, hashOrQuery) : s.slice(8, hashOrQuery);
                return `inline vless://…@${hostPart}`;
            }
            return s.length > 80 ? s.slice(0, 77) + "…" : s;
        },

        // ---- API helpers ----

        async _fetchJson(url, opts) {
            const r = await fetch(url, opts);
            const text = await r.text();
            let data;
            try { data = JSON.parse(text); } catch { data = { error: text || `HTTP ${r.status}` }; }
            if (!r.ok) {
                throw new Error(data.error || `HTTP ${r.status}`);
            }
            return data;
        },

        async refresh(silent) {
            try {
                const s = await this._fetchJson("/api/state");
                this.state = s;
                if (!silent) this.error = "";
            } catch (e) {
                if (!silent) this.error = e.message;
            }
        },

        async _send(method, path, body) {
            this.busy = true;
            this.error = "";
            try {
                const opts = { method };
                if (body !== undefined && body !== null) {
                    opts.headers = { "Content-Type": "application/json" };
                    opts.body = JSON.stringify(body);
                }
                const data = await this._fetchJson(path, opts);
                this.state = data;
            } catch (e) {
                this.error = e.message;
            } finally {
                this.busy = false;
            }
        },

        // ---- Actions: subscriptions ----

        async submitAddSubscription() {
            const source = this.newSubSource.trim();
            if (!source) return;
            // Label is optional — backend derives a default from the source
            // hostname if we send empty.
            await this._send("POST", "/api/subscriptions",
                { label: this.newSubLabel.trim(), source });
            if (!this.error) {
                this.newSubLabel = "";
                this.newSubSource = "";
                this.showAddForm = false;
                // Auto-pick first server in the new subscription if nothing is
                // selected yet — convenient onboarding.
                if (this.state && !this.state.active_server) {
                    const added = this.state.subscriptions[this.state.subscriptions.length - 1];
                    if (added && added.servers.length > 0) {
                        await this.pickServer(added.id, added.servers[0].id);
                    }
                }
            }
        },

        cancelAddForm() {
            this.showAddForm = false;
            this.newSubLabel = "";
            this.newSubSource = "";
            this.error = "";
        },

        async refreshSubscription(id) {
            await this._send("POST", `/api/subscriptions/${encodeURIComponent(id)}/refresh`);
        },

        async renameSubscription(sub) {
            const next = window.prompt("New label:", sub.label);
            if (!next || next.trim() === sub.label) return;
            await this._send("PATCH", `/api/subscriptions/${encodeURIComponent(sub.id)}`,
                { label: next.trim() });
        },

        async deleteSubscription(sub) {
            const a = this.state.active_server;
            const willClearActive = a && a.subscription_id === sub.id;
            const warning = willClearActive
                ? `Delete "${sub.label}"? VPN will turn off — the active server is in this subscription.`
                : `Delete "${sub.label}"?`;
            if (!window.confirm(warning)) return;
            await this._send("DELETE", `/api/subscriptions/${encodeURIComponent(sub.id)}`);
        },

        // ---- Actions: server selection / VPN ----

        async pickServer(subId, srvId) {
            if (this.isActive(subId, srvId)) return;
            await this._send("POST", "/api/server",
                { subscription_id: subId, server_id: srvId });
        },

        async toggleVpn(on) {
            await this._send("POST", "/api/toggle", { on });
        },

        // ---- Actions: rules ----

        async setRulesUrl(url) {
            await this._send("POST", "/api/rules-url", { url: url || null });
        },

        async refreshRules() {
            await this._send("POST", "/api/rules/refresh");
        },

        // ---- Utils ----

        fmtTime(iso) {
            if (!iso) return "never";
            const d = new Date(iso);
            if (isNaN(d)) return iso;
            return d.toLocaleString();
        },
    };
}
