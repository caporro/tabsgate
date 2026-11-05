// The UI's only gateway integration. Credentials live in this instance's memory;
// requests never use cookies, browser storage or credential-bearing URLs.
export class APIError extends Error {
  constructor(code, status = 0, retryAfter = 0, uncertain = false) {
    super(code);
    this.code = code;
    this.status = status;
    this.retryAfter = retryAfter;
    this.uncertain = uncertain;
  }
}

export class GateAPI {
  #credentials = null;
  #connectionID = null;
  #pending = new Set();
  #generation = 0;
  #base = new URL('./', document.baseURI);

  get baseURL() { return new URL('v1', this.#base).href.replace(/\/$/, ''); }

  close() {
    this.#generation++;
    for (const controller of this.#pending) controller.abort();
    this.#pending.clear();
    this.#credentials = null;
    this.#connectionID = null;
  }

  async #request(path, { method = 'GET', body, management = false, issuing = false } = {}) {
    const controller = new AbortController();
    this.#pending.add(controller);
    const timeout = setTimeout(() => controller.abort(), 35000);
    try {
      const headers = new Headers({ Accept: 'application/json' });
      if (body !== undefined) headers.set('Content-Type', 'application/json');
      if (management) {
        if (!this.#credentials) throw new APIError('connection_closed');
        headers.set('Authorization', 'Bearer ' + this.#credentials.api_key);
        // Fetch uses a ByteString for headers; send the same UTF-8 bytes that
        // registration JSON and the existing Go management API use for a pass.
        headers.set('X-TabsGate-Keyword', String.fromCharCode(...new TextEncoder().encode(this.#credentials.keyword)));
      }
      const response = await fetch(new URL(path, this.#base), {
        method, headers, body: body === undefined ? undefined : JSON.stringify(body),
        signal: controller.signal, credentials: 'omit', cache: 'no-store',
        redirect: 'error', mode: 'same-origin', referrerPolicy: 'no-referrer',
      });
      if (response.status === 204) return null;
      let result;
      try { result = await response.json(); }
      catch { throw new APIError('invalid_response', response.status, 0, issuing && response.ok); }
      if (!response.ok) {
        throw new APIError(result?.error?.code || 'request_failed', response.status,
          Number(response.headers.get('Retry-After')) || 0, issuing && response.status >= 500);
      }
      return result;
    } catch (error) {
      if (error instanceof APIError) throw error;
      throw new APIError(controller.signal.aborted ? 'request_timeout' : 'network_error', 0, 0, issuing);
    } finally {
      clearTimeout(timeout);
      this.#pending.delete(controller);
    }
  }

  async providers() {
    const result = await this.#request('api/v1/providers');
    if (!Array.isArray(result?.data) || !result.data.length) throw new APIError('no_providers');
    return result.data;
  }

  async open(provider, apiKey, keyword) {
    const generation = this.#generation;
    const connection = await this.#request('api/v1/connections', {
      method: 'POST', body: { provider, api_key: apiKey, keyword },
    });
    if (generation !== this.#generation) throw new APIError('connection_closed');
    if (typeof connection?.id !== 'string') throw new APIError('invalid_response');
    this.#credentials = { api_key: apiKey, keyword };
    this.#connectionID = connection.id;
    return connection;
  }

  #path(suffix) {
    if (!this.#connectionID) throw new APIError('connection_closed');
    return 'api/v1/connections/' + encodeURIComponent(this.#connectionID) + suffix;
  }

  async keys() {
    const result = await this.#request(this.#path('/keys'), { management: true });
    if (!Array.isArray(result?.data)) throw new APIError('invalid_response');
    return result.data;
  }

  async models() {
    const result = await this.#request(this.#path('/models'), { management: true });
    if (!Array.isArray(result?.data)) throw new APIError('invalid_response');
    return [...new Set(result.data.map(item => item.id).filter(id => typeof id === 'string'))].sort();
  }

  create(limits) {
    return this.#request(this.#path('/keys'), { method: 'POST', body: limits, management: true, issuing: true });
  }

  edit(id, limits) {
    return this.#request(this.#path('/keys/' + encodeURIComponent(id)), { method: 'PATCH', body: limits, management: true });
  }

  revoke(id) {
    return this.#request(this.#path('/keys/' + encodeURIComponent(id)), { method: 'DELETE', management: true });
  }

  async updateConnection(changes) {
    const generation = this.#generation;
    const connection = await this.#request(this.#path(''), { method: 'PATCH', body: changes, management: true });
    if (generation !== this.#generation) throw new APIError('connection_closed');
    if (typeof connection?.id !== 'string' || connection.id !== this.#connectionID) throw new APIError('invalid_response');
    this.#credentials = { ...this.#credentials, ...changes };
    // Model discovery started with the old upstream key is no longer relevant.
    for (const controller of this.#pending) controller.abort();
    return connection;
  }

  deleteConnection() {
    return this.#request(this.#path(''), { method: 'DELETE', management: true });
  }
}
