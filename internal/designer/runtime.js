// The browser and bundler pieces Designer's codec code expects, and the
// functions ampiocli calls. Runs in goja before the codec program.

var window = globalThis;

// Designer logs as it works; ampiocli reports errors through return values instead.
var console = { log() {}, info() {}, debug() {}, warn() {}, error() {} };

// zustand (a small state-store library) as far as the codec uses it: the
// codec keeps its device registry in a store.
var zustand = (() => {
  const createStore = (init) => {
    let state;
    const listeners = new Set();
    const setState = (partial, replace) => {
      const next = typeof partial === "function" ? partial(state) : partial;
      if (Object.is(next, state)) return;
      const prev = state;
      state = (replace ?? (typeof next !== "object" || next === null)) ? next : Object.assign({}, state, next);
      listeners.forEach((l) => l(state, prev));
    };
    const getState = () => state;
    const subscribe = (l) => (listeners.add(l), () => listeners.delete(l));
    const api = { setState, getState, getInitialState: () => initial, subscribe };
    const initial = (state = init(setState, getState, api));
    return api;
  };
  const create = (init) => {
    if (!init) return create;
    const api = createStore(init);
    const useStore = (select = (s) => s) => select(api.getState());
    return Object.assign(useStore, api);
  };
  return { create, createStore };
})();

// once makes a chunk's module function run once, however many chunks import it.
var once = (f) => {
  let result;
  return () => (result ??= f());
};

var importShared = (name) => {
  if (name === "zustand") return zustand;
  throw new Error("Designer changed: the codec needs shared module " + name);
};

// Fields Designer derives from the bytes or from a rule's position; they are not part of a rule.
var derived = new Set(["uuid", "macUniq", "ordinalNo", "hash", "originalCond", "legacyHash"]);

// codec wraps Designer's functions. devices is the device_api/from/list
// "devices" array as JSON; rules cross as JSON, bytes as ArrayBuffers.
function codec(designer, devices) {
  designer.init();
  JSON.parse(devices).forEach((row) => designer.addDevice(row));
  const clean = (rule) => Object.fromEntries(Object.entries(rule).filter(([k]) => !derived.has(k)));
  return {
    // Rules with several inputs point into the module's linked table, which
    // Designer keeps in its registry: set it to the table being decoded with.
    decode: (mac, bytes, linked) => {
      designer.registry.updateLinkedCondArrByMacUniq(mac, Array.from(new Uint8Array(linked)));
      return JSON.stringify(designer.decode(mac, Array.from(new Uint8Array(bytes))).map(clean));
    },

    // Same path as Designer's own save, minus the publish; the encoder
    // rebuilds the linked table. originalCond is emptied so every rule is
    // encoded from its fields, never copied from old bytes.
    encode: (mac, rules) => {
      const bytes = designer.encode(mac, JSON.parse(rules).map((r) => ({ ...r, macUniq: mac, originalCond: "" })));
      const linked = designer.registry.getLinkedCondArr(mac) ?? [];
      return [new Uint8Array(bytes).buffer, new Uint8Array(linked).buffer];
    },
  };
}
