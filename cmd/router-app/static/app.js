// Alpine.js component for yonder.
//
// Single state machine: keep the latest server-side state in `state`,
// refetch after every mutation, surface transient `error` / `lastAction`.

function vpnui() {
    return {
        // Server-side snapshot (null until first load)
        state: null,

        // Onboarding form fields
        subscriptionUrl: "",
        rulesUrl: "",
        newRulesUrl: "",

        // UI flags
        busy: false,
        error: "",
        lastAction: "",

        // ---- Lifecycle ----

        init() {
            this.refresh();
            // Light polling so manual changes (curl from another device) show up.
            setInterval(() => { if (!this.busy) this.refresh(true); }, 10000);
        },

        // ---- Computed ----

        get activeServerName() {
            if (!this.state || !this.state.active_server_id) return "";
            const s = (this.state.servers || []).find(s => s.id === this.state.active_server_id);
            return s ? s.name : this.state.active_server_id;
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

        async _post(path, body) {
            this.busy = true;
            this.error = "";
            this.lastAction = "";
            try {
                const data = await this._fetchJson(path, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body || {}),
                });
                this.state = data;
                this.lastAction = data.last_action || "";
                // Auto-clear the success message after a few seconds.
                if (this.lastAction) setTimeout(() => { this.lastAction = ""; }, 5000);
            } catch (e) {
                this.error = e.message;
            } finally {
                this.busy = false;
            }
        },

        // ---- Actions ----

        async submitSubscription() {
            if (!this.subscriptionUrl) return;
            await this._post("/api/subscription", { url: this.subscriptionUrl });
            // After fetching servers, store the optional rules URL too.
            if (!this.error && this.rulesUrl) {
                await this._post("/api/rules-url", { url: this.rulesUrl });
            }
            // Auto-pick first server so the user has something selected.
            if (!this.error && this.state && this.state.servers.length > 0
                && !this.state.active_server_id) {
                await this._post("/api/server", { id: this.state.servers[0].id });
            }
            this.subscriptionUrl = "";
            this.rulesUrl = "";
        },

        async refreshSubscription() {
            if (!this.state || !this.state.subscription_url) return;
            await this._post("/api/subscription", { url: this.state.subscription_url });
        },

        async resetSubscription() {
            if (!confirm("Replace the current subscription? This will clear all servers.")) return;
            // Sending an empty/invalid URL won't help; instead we reset servers
            // by going through the onboarding flow. Cheapest UX: clear active +
            // ask user to paste a fresh URL via the onboarding screen.
            // We do that by emptying servers via state — but the server doesn't
            // have a "wipe" endpoint yet. Simpler: prompt for a new URL inline.
            const url = prompt("New subscription URL:");
            if (!url) return;
            await this._post("/api/subscription", { url });
        },

        async pickServer(id) {
            if (this.state && id === this.state.active_server_id) return;
            await this._post("/api/server", { id });
        },

        async toggleVpn(on) {
            await this._post("/api/toggle", { on });
        },

        async setRulesUrl(url) {
            await this._post("/api/rules-url", { url: url || null });
        },

        async refreshRules() {
            await this._post("/api/rules/refresh", {});
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
