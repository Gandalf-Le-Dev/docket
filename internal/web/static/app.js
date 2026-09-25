// Images pasted, dropped or picked into a body upload to /image and land in
// the text as the marker the server draws; a swap that leaves what it
// brought out of sight scrolls to it; ages like "3h ago" keep counting; the
// search runs as it is typed; a title edits in place; the forms keep drafts;
// an action's report leaves the address bar once shown; and a page
// refreshes itself when an item changes anywhere (see live). Bodies
// arrive with htmx swaps as well as with the page, so every listener sits on
// the document and finds its field when the event comes, rather than
// binding to fields once at load.
(() => {
  "use strict";

  // store.MaxImageBytes: checked here too, because the server cuts a bigger
  // upload off mid-body and the browser then reports a bare network error.
  const maxImageBytes = 5 << 20;

  let uploads = 0;

  function images(list) {
    return [...(list || [])].filter((f) => f.type.startsWith("image/"));
  }

  function submitButtons(form) {
    const inside = form.querySelectorAll('button[type="submit"], button:not([type])');
    const outside = form.id ? document.querySelectorAll(`button[form="${form.id}"]`) : [];
    return [...inside, ...outside];
  }

  function setBusy(form, delta) {
    form.dataset.uploads = String(Number(form.dataset.uploads || 0) + delta);
    const busy = form.dataset.uploads !== "0";
    for (const b of submitButtons(form)) b.disabled = busy;
  }

  // insertLine returns exactly what it inserted, so a failed upload can take
  // back the line breaks along with the placeholder.
  function insertLine(ta, text) {
    const { selectionStart: start, selectionEnd: end, value } = ta;
    const before = start > 0 && value[start - 1] !== "\n" ? "\n" : "";
    const after = value[end] === "\n" ? "" : "\n";
    ta.setRangeText(before + text + after, start, end, "end");
    if (!after) ta.selectionStart = ta.selectionEnd = ta.selectionEnd + 1;
    return before + text + after;
  }

  function replaceText(ta, from, to) {
    const i = ta.value.indexOf(from);
    if (i < 0) return false;
    ta.setRangeText(to, i, i + from.length, "preserve");
    return true;
  }

  async function post(file) {
    if (file.size > maxImageBytes) throw new Error(`image exceeds ${maxImageBytes >> 20} MB`);
    const body = new FormData();
    body.append("image", file);
    const res = await fetch("/image", { method: "POST", body, credentials: "same-origin" });
    if (res.redirected) throw new Error("signed out: sign in again, then attach the image");
    let data = {};
    try {
      data = await res.json();
    } catch {}
    if (!res.ok || !data.markdown) throw new Error(data.error || `upload failed (${res.status})`);
    return data.markdown;
  }

  function upload(ta, status, files) {
    const form = ta.form;
    status.textContent = "";
    for (const file of files) {
      // numbered, so each upload finds its own placeholder whatever order they finish in
      const placeholder = `![Uploading image ${++uploads}…]()`;
      const inserted = insertLine(ta, placeholder);
      fit(ta);
      if (form) setBusy(form, 1);
      post(file)
        .then((markdown) => replaceText(ta, placeholder, markdown))
        .catch((err) => {
          // the line breaks are left if the text around the placeholder was edited meanwhile
          replaceText(ta, inserted, "") || replaceText(ta, placeholder, "");
          status.textContent = err.message;
        })
        .finally(() => {
          fit(ta);
          if (form) {
            setBusy(form, -1);
            saveDraft(form);
          }
        });
    }
  }

  function bodyOf(target) {
    const ta = target instanceof Element ? target.closest("textarea[name=body]") : null;
    const attach = ta && ta.id ? document.querySelector(`.attach[data-for="${ta.id}"]`) : null;
    return attach ? { ta, attach, status: attach.querySelector(".upload-status") } : null;
  }

  const sizesItself = window.CSS && CSS.supports("field-sizing", "content");
  function fit(ta) {
    if (sizesItself || !ta.matches(".form textarea, .retitle textarea")) return;
    // collapsing the field to measure it would scroll whatever holds it
    const scroller = ta.closest(".drawer .body") || document.scrollingElement;
    const top = scroller.scrollTop;
    ta.style.height = "0";
    const style = getComputedStyle(ta);
    const borders = parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
    ta.style.height = `${ta.scrollHeight + borders}px`;
    scroller.scrollTop = top;
  }

  function reveal(root) {
    for (const attach of root.querySelectorAll(".attach[hidden]")) attach.hidden = false;
    for (const ta of root.querySelectorAll(".form textarea")) fit(ta);
  }
  reveal(document);
  document.addEventListener("htmx:load", (e) => reveal(e.target));
  document.addEventListener("input", (e) => {
    if (e.target instanceof HTMLTextAreaElement) fit(e.target);
  });

  document.addEventListener("click", (e) => {
    const button = e.target instanceof Element && e.target.closest(".attach button");
    if (button) button.parentElement.querySelector("input[type=file]").click();
  });

  document.addEventListener("change", (e) => {
    const picker = e.target;
    const attach = picker instanceof HTMLInputElement && picker.type === "file" && picker.closest(".attach");
    if (!attach) return;
    const ta = document.getElementById(attach.dataset.for);
    if (ta) upload(ta, attach.querySelector(".upload-status"), images(picker.files));
    picker.value = "";
  });

  document.addEventListener("paste", (e) => {
    const field = bodyOf(e.target);
    const files = images(e.clipboardData && e.clipboardData.files);
    if (!field || !files.length) return;
    e.preventDefault();
    upload(field.ta, field.status, files);
  });

  const dragsFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");
  document.addEventListener("dragover", (e) => {
    const field = bodyOf(e.target);
    if (!field || !dragsFiles(e)) return;
    e.preventDefault();
    field.ta.classList.add("drop-target");
  });
  document.addEventListener("dragleave", (e) => {
    const field = bodyOf(e.target);
    if (field) field.ta.classList.remove("drop-target");
  });
  document.addEventListener("drop", (e) => {
    const field = bodyOf(e.target);
    if (!field) return;
    field.ta.classList.remove("drop-target");
    if (!dragsFiles(e)) return;
    // left alone, the browser opens a dropped file in place of the page and the draft is lost
    e.preventDefault();
    const files = images(e.dataTransfer.files);
    if (!files.length) {
      field.status.textContent = "only images can be attached";
      return;
    }
    field.ta.focus();
    upload(field.ta, field.status, files);
  });

  // What is typed into the new-entry form or an entry's edit form is kept in
  // this browser as it is typed, and comes back when that form opens again:
  // after the drawer was closed, a reload, a failed save. The drawer and the
  // entry's page share an entry's draft, named by the entry's id and filing
  // time so a database started afresh cannot hand it to another entry. Only
  // the fields typed into are kept: the rest show the entry as it is now, a
  // body an agent rewrote meanwhile, or the scope a "+ in" link started in.
  // Signing out deletes every draft, and none is kept after it. Storage can
  // be missing or refuse (a private window, a full quota), and the forms then
  // work as they would without drafts.
  const draftFields = ["title", "body", "scope"];
  const uploading = /!\[Uploading image \d+…\]\(\)\n?/g;
  // the server keeps an image no saved body shows for this long
  // (cmd/docket's imageGrace), so an older draft's images may be gone
  const draftLife = 7 * 86400e3;
  const draftKey = (form) => `docket.draft.${form.dataset.draft}`;
  function readDraft(form) {
    try {
      const draft = JSON.parse(localStorage.getItem(draftKey(form)));
      return draft && typeof draft.fields === "object" ? draft : null;
    } catch {
      return null;
    }
  }
  function dropDraft(form) {
    try {
      localStorage.removeItem(draftKey(form));
    } catch {}
  }
  function dropDrafts(before) {
    try {
      for (let i = localStorage.length - 1; i >= 0; i--) {
        const key = localStorage.key(i);
        if (!key || !key.startsWith("docket.draft.")) continue;
        let at = 0;
        try {
          at = JSON.parse(localStorage.getItem(key)).at;
        } catch {}
        if (!(at > before)) localStorage.removeItem(key);
      }
    } catch {}
  }
  dropDrafts(Date.now() - draftLife);
  // Whoever uses this browser next must not find what was typed before.
  let signedOut = false;
  document.addEventListener(
    "submit",
    (e) => {
      if (!(e.target instanceof HTMLFormElement) || !e.target.matches("[data-sign-out]")) return;
      // an upload still running would save its form's draft when it lands
      signedOut = true;
      dropDrafts(Infinity);
    },
    true,
  );

  // A draft remembers which version of the entry it started from (the base
  // an edit form posts, see web.go's update), so its note can say when the
  // entry has changed since.
  const baseOf = (form) => form.elements.namedItem("base")?.value ?? "";
  // What the entry holds, as the edit form posts it in orig_<field>: a
  // refused save's form shows the typed text as its fields' own, and a draft
  // must hold every field that differs from the entry, not from that text.
  // The new-entry form has no entry; its fields start as it was served.
  function savedOf(form, field) {
    const orig = form.elements.namedItem(`orig_${field.name}`);
    return orig ? orig.value.replace(/\r\n/g, "\n") : field.defaultValue;
  }
  function saveDraft(form) {
    if (!form.dataset.draft || signedOut) return;
    const typed = {};
    for (const name of draftFields) {
      const field = form.elements.namedItem(name);
      const value = field && field.value.replace(uploading, "");
      if (field && value !== savedOf(form, field)) typed[name] = value;
    }
    if (!Object.keys(typed).length) return dropDraft(form);
    const base = readDraft(form)?.base ?? baseOf(form);
    try {
      // when typing began: no image uploaded into it is older, so it goes first
      const at = readDraft(form)?.at ?? Date.now();
      localStorage.setItem(draftKey(form), JSON.stringify({ fields: typed, base, at }));
    } catch {}
  }

  function restoreDraft(form) {
    const draft = readDraft(form);
    // htmx's first load event covers the page this script already did
    if (!draft || form.querySelector(".draft-note")) return;
    let restored = false;
    for (const name of draftFields) {
      const field = form.elements.namedItem(name);
      const value = draft.fields[name];
      if (!field || typeof value !== "string" || value === field.value) continue;
      field.value = value;
      if (field instanceof HTMLTextAreaElement) fit(field);
      restored = true;
    }
    // a refused save's form already shows the text the draft holds
    if (!restored) return form.hasAttribute("data-unsaved") || dropDraft(form);
    const changed = draft.base && baseOf(form) && draft.base !== baseOf(form);
    const text = document.createElement("span");
    const discard = document.createElement("button");
    discard.type = "button";
    discard.className = "btn--quiet";
    discard.textContent = "Discard";
    discard.addEventListener("click", () => {
      dropDraft(form);
      for (const name of draftFields) {
        const field = form.elements.namedItem(name);
        if (!field) continue;
        field.value = savedOf(form, field);
        if (field instanceof HTMLTextAreaElement) fit(field);
      }
      note.remove();
    });
    const note = document.createElement("p");
    note.className = "draft-note";
    note.setAttribute("role", "status");
    note.append(text, discard);
    (form.querySelector("label") || form.firstChild)?.before(note);
    // a status region speaks what changes in it, not what it was born with
    setTimeout(() => {
      text.textContent = changed ? "Draft restored. The entry has changed since you started it." : "Draft restored.";
    });
  }
  function restoreDrafts(root) {
    for (const form of root.querySelectorAll("form[data-draft]")) restoreDraft(form);
  }
  restoreDrafts(document);
  document.addEventListener("htmx:load", (e) => restoreDrafts(e.target));
  document.addEventListener("input", (e) => {
    const form = e.target.form;
    if (form && draftFields.includes(e.target.name)) saveDraft(form);
  });
  // Only an answer that reports the save (web.go's did) drops the draft: a
  // failed save, Cancel and a closed drawer keep it. The form is named by the
  // request: the swap has taken it out, so the event's elt is the body.
  document.addEventListener("htmx:afterRequest", (e) => {
    const { requestConfig, successful, xhr } = e.detail;
    const form = requestConfig && requestConfig.elt;
    if (!(form instanceof HTMLFormElement) || !form.dataset.draft || !successful) return;
    const did = new URL(xhr.responseURL).searchParams.get("did");
    if (did === "saved" || did === "added") dropDraft(form);
  });

  // A port of agoAt in web.go, rule for rule, so a page left open keeps its
  // ages true: change both together (TestAgoInScript runs this on the cases
  // TestAgoThresholds pins). The date is the stamp's own, in the offset it
  // was written with.
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  function ago(stamp, now) {
    const t = Date.parse(stamp);
    if (Number.isNaN(t)) return stamp;
    const d = now - t;
    if (d < 60e3) return "just now";
    if (d < 3600e3) return `${Math.floor(d / 60e3)}m ago`;
    if (d < 86400e3) return `${Math.floor(d / 3600e3)}h ago`;
    if (d < 30 * 86400e3) return `${Math.floor(d / 86400e3)}d ago`;
    const zone = /([+-])(\d\d):(\d\d)$/.exec(stamp);
    const offset = zone ? (zone[1] === "-" ? -1 : 1) * (Number(zone[2]) * 60 + Number(zone[3])) * 60e3 : 0;
    const day = new Date(t + offset);
    return `${months[day.getUTCMonth()]} ${day.getUTCDate()}, ${day.getUTCFullYear()}`;
  }
  function tick() {
    const now = Date.now();
    for (const el of document.querySelectorAll("time[data-ago]")) {
      const text = ago(el.getAttribute("datetime"), now);
      if (el.textContent !== text) el.textContent = text;
    }
  }
  setInterval(tick, 60e3);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) tick();
  });

  // The title's button turns it into a field in place. Enter or leaving it
  // saves the title alone (web.go's update keeps the body and scope as they
  // are now); Escape, or saving it unchanged or empty, puts the title back,
  // and focus returns to the button. A title the server refuses, too long
  // say, opens the field again with what was typed, beside the reason; a
  // save refused because the title changed meanwhile comes back as the full
  // edit form instead (web.go's refused).
  const maxTitle = 500; // store.MaxTitleBytes, counted in characters here
  function retitle(button, typed) {
    const heading = button.closest("h1");
    const form = heading && heading.closest("form");
    if (!form || heading.hidden) return;
    const was = button.textContent.trim();
    // a textarea, so a long title wraps as the heading did on a narrow screen
    const field = document.createElement("textarea");
    field.rows = 1;
    field.name = "title";
    field.required = true;
    field.maxLength = maxTitle;
    field.defaultValue = was;
    field.value = typed ?? was;
    field.setAttribute("aria-label", "Title");
    const style = getComputedStyle(heading);
    for (const p of ["fontFamily", "fontSize", "fontWeight", "lineHeight", "letterSpacing", "marginBottom"]) {
      field.style[p] = style[p];
    }
    heading.hidden = true;
    heading.after(field);
    fit(field);
    field.focus();
    field.setSelectionRange(field.value.length, field.value.length);

    let settled = false;
    const cancel = () => {
      settled = true;
      field.remove();
      heading.hidden = false;
      button.focus();
    };
    const save = () => {
      if (settled) return;
      const title = field.value.replace(/\s*\n\s*/g, " ").trim();
      if (!title || title === was) return cancel();
      settled = true;
      field.value = title;
      form.addEventListener(
        "htmx:afterRequest",
        (ev) => {
          // one that never reached the server can be tried again as it is
          if (!ev.detail.successful) {
            settled = false;
            return;
          }
          const refused = new URL(ev.detail.xhr.responseURL).searchParams.has("err");
          const again = document.querySelector("#page [data-retitle]");
          if (!again) return;
          if (refused) retitle(again, title);
          else again.focus({ preventScroll: true });
        },
        { once: true },
      );
      form.requestSubmit();
    };
    field.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" && !ev.isComposing) {
        ev.preventDefault();
        save();
      } else if (ev.key === "Escape") {
        ev.preventDefault();
        if (!settled) cancel();
      }
    });
    // the window losing focus to another tab or app is not leaving the title
    field.addEventListener("blur", () => {
      if (document.activeElement !== field) save();
    });
  }
  document.addEventListener("click", (e) => {
    const button = e.target instanceof Element && e.target.closest("[data-retitle]");
    if (button) retitle(button);
  });

  const liveHeader = "Docket-Live";
  const isLive = (config) => Boolean(config && config.headers && config.headers[liveHeader]);
  const searchHeader = "Docket-Search";
  const isSearch = (config) => Boolean(config && config.headers && config.headers[searchHeader]);

  // Swaps from the list keep its scroll, which can leave what they brought out
  // of sight: a failed action's message at the top of the list, or the drawer,
  // which a narrow screen stacks above the list. A live refresh (see live)
  // brought nothing anyone asked to see, so it stays where the reader is.
  document.addEventListener("htmx:afterSettle", (e) => {
    if (isLive(e.detail.requestConfig)) return;
    const shown = document.querySelector("#page .error") || document.querySelector(".drawer");
    if (!shown) return;
    const { top } = shown.getBoundingClientRect();
    if (top < 0 || top > window.innerHeight) shown.scrollIntoView();
  });

  // The fingerprint this page's stylesheet and scripts were loaded with. A
  // swap never touches <head>, so after a deploy only a load brings new ones.
  const asset = new URL(document.currentScript.src).searchParams.get("v");

  // Some answers must be loaded as a page rather than swapped: one from a
  // later deploy, whose #page may need the new stylesheet and scripts; a
  // boosted link that led off the pages this script knows, to /healthz say,
  // which has no #page and would blank the screen; and a failed link (a
  // pasted /todo/999), which htmx would not swap at all, so the browser shows
  // the server's error. Posts come back to a page and say what went wrong there.
  document.addEventListener("htmx:beforeSwap", (e) => {
    const { boosted, requestConfig, xhr, serverResponse, isError } = e.detail;
    if (!boosted && !isSearch(requestConfig)) return;
    const page = new DOMParser().parseFromString(serverResponse, "text/html").getElementById("page");
    const stale = page && !isError && page.dataset.asset !== asset;
    const offPage = requestConfig.verb === "get" && (isError || !page);
    if (!stale && !offPage) return;
    e.preventDefault();
    location.href = xhr.responseURL || requestConfig.path;
  });
  document.addEventListener("htmx:historyRestore", () => {
    const page = document.getElementById("page");
    if (page && page.dataset.asset !== asset) location.reload();
  });

  // A page arrives at a URL that reports what an action did (?did=closed and
  // the like), and names the view itself in data-here (web.go's here). The
  // toast is on screen by then, so the report leaves the address bar: a
  // reload, a bookmark or the theme switch must not show it again.
  function tidyURL() {
    const here = document.getElementById("page")?.dataset.here;
    if (here && here !== location.pathname + location.search) {
      history.replaceState(history.state, "", here + location.hash);
    }
  }
  tidyURL();
  document.addEventListener("htmx:afterSwap", tidyURL);
  document.addEventListener("htmx:historyRestore", tidyURL);

  // Capture, and stop there: htmx submits a boosted form from its own
  // listener on the form, which never checks whether the event was cancelled.
  document.addEventListener(
    "submit",
    (e) => {
      if ((e.target.dataset.uploads || "0") === "0") return;
      e.preventDefault();
      e.stopPropagation();
    },
    true,
  );

  // The list follows the search field as it is typed in, a quarter second
  // after the last key. Each search replaces the address rather than adding
  // to history, so back leaves the list instead of stepping through every
  // prefix typed; one begun on an entry's page leaves that page, so it adds
  // one entry and back returns there. Enter runs the search at once. One
  // search runs at a time, and one asked for meanwhile waits for it to land,
  // then compares with the address it left: htmx's own queue would swap the
  // late one into the #page the first had already replaced.
  let searchTimer = 0;
  let searching = false;
  let searchAgain = false;
  let searchXhr = null;
  function search() {
    clearTimeout(searchTimer);
    searchTimer = 0;
    if (searching) {
      searchAgain = true;
      return;
    }
    const form = document.querySelector("#page form[role=search]");
    if (!form || !window.htmx) return;
    const params = new URLSearchParams();
    for (const [k, v] of new FormData(form)) if (v !== "") params.append(k, v);
    params.sort();
    const current = new URLSearchParams(location.search);
    current.sort();
    const path = new URL(form.action).pathname;
    if (path === location.pathname && params.toString() === current.toString()) return;
    const url = params.toString() ? `${path}?${params}` : path;
    searching = true;
    htmx
      .ajax("GET", url, {
        // not the body, whose requests are the live refresh's (see refresh)
        source: document.documentElement,
        target: "#page",
        select: "#page",
        swap: "outerHTML",
        headers: { [searchHeader]: "1" },
        [path === location.pathname ? "replace" : "push"]: url,
      })
      // an abort, when a link overtakes the search, is the search's end
      .catch(() => {})
      .finally(() => {
        searching = false;
        searchXhr = null;
        if (searchAgain) {
          searchAgain = false;
          search();
        }
      });
  }
  // Text typed into a form that keeps no draft, a drop's reason or a scope's
  // new name, would go with the page a search brings; the typed search waits
  // while that text is there, and asks again at each change to it or when
  // focus moves. So does a refused save's form, whose text is saved nowhere.
  // Enter still searches at once.
  let searchHeld = false;
  function unsaved() {
    if (document.querySelector("#page form[data-unsaved]")) return true;
    for (const form of document.querySelectorAll("#page form:not([role=search], [data-draft])")) {
      for (const field of form.querySelectorAll("input[type=text], textarea")) {
        if (field.value !== field.defaultValue) return true;
      }
    }
    return false;
  }
  function searchSoon() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      searchTimer = 0;
      searchHeld = unsaved();
      if (!searchHeld) search();
    }, 250);
  }
  const searchField = (el) =>
    el instanceof HTMLInputElement && el.matches("#page form[role=search] input[type=search]");
  // an input method's text is not typed until its composition ends
  document.addEventListener("input", (e) => {
    if (searchField(e.target) ? window.htmx && !e.isComposing : searchHeld) searchSoon();
  });
  document.addEventListener("focusout", () => {
    if (searchHeld) searchSoon();
  });
  document.addEventListener("compositionend", (e) => {
    if (searchField(e.target) && window.htmx) searchSoon();
  });
  // A link or form taken while a search is due or on its way goes where it
  // was asked to: the search would land after it and take the page back to
  // the list.
  document.addEventListener("htmx:beforeSend", (e) => {
    if (isSearch(e.detail.requestConfig)) searchXhr = e.detail.xhr;
  });
  document.addEventListener("htmx:beforeRequest", (e) => {
    const config = e.detail.requestConfig;
    if (isSearch(config) || isLive(config)) return;
    clearTimeout(searchTimer);
    searchTimer = 0;
    searchAgain = false;
    searchHeld = false;
    if (searchXhr) searchXhr.abort();
  });
  document.addEventListener(
    "submit",
    (e) => {
      if (!window.htmx || !e.target.matches("#page form[role=search]")) return;
      e.preventDefault();
      e.stopPropagation();
      search();
    },
    true,
  );

  // A swap replaces the search field while it is being typed in: its
  // results arrive, or the page refreshes. The new field keeps the focus,
  // the caret and whatever was typed after the request left, and text the
  // page did not search for gets its search.
  let typing = null;
  document.addEventListener("htmx:beforeSwap", () => {
    const el = document.activeElement;
    typing = searchField(el)
      ? { el, value: el.value, start: el.selectionStart, end: el.selectionEnd, dir: el.selectionDirection }
      : null;
  });
  document.addEventListener("htmx:afterSwap", () => {
    const was = typing;
    typing = null;
    const q = document.querySelector("#page form[role=search] input[type=search]");
    if (!was || was.el.isConnected || !q) return;
    q.value = was.value;
    q.focus({ preventScroll: true });
    q.setSelectionRange(was.start, was.end, was.dir);
    if (q.value !== q.defaultValue && !searchTimer && !searching) searchSoon();
  });

  if (document.getElementById("page") && window.htmx && window.EventSource) live();

  // /events says when an item changed anywhere: an agent, another tab, another
  // device. The page then fetches its own URL again and swaps #page as a
  // boosted link would, without a history entry, so what it shows is always
  // what that URL renders now. It waits while a swap would cost someone
  // something: a form being filled in, a hidden tab, a request of its own.
  function live() {
    let latest = "";
    let force = false; // the stream was refused, and only a refresh says why
    let running = false;
    let failed = false;
    let later = 0;
    let failDelay = 1000;
    let lastRefresh = 0;
    let overtaken = false;
    let debounce = 0;
    let burst = 0;
    let before = null;

    // Looked up each time: a history restore replaces the body's children.
    function say(text) {
      let note = document.getElementById("live-note");
      if (!note) {
        note = document.createElement("div");
        note.id = "live-note";
        note.className = "live-note";
        note.setAttribute("role", "status");
        document.body.append(note);
      }
      note.textContent = text;
    }
    // A live region must exist, empty, before its first message is announced.
    say("");

    // A browser without :popover-open throws on the selector; it has no
    // popovers open either.
    function popoverOpen() {
      try {
        return Boolean(document.querySelector("#page [popover]:popover-open"));
      } catch {
        return false;
      }
    }

    // A form someone is filling in, which a swap would throw away. One that
    // is open but untouched and unfocused is not held: the refresh brings it
    // back with the item's new values. The search field holds only while it
    // has text its search has not applied yet, which lasts until the search
    // runs: that search brings a newer page than the refresh would. A refused
    // save's form holds whatever its fields say: its text is not saved.
    function held() {
      const active = document.activeElement;
      if (popoverOpen()) return true;
      for (const form of document.querySelectorAll("#page form")) {
        if (form.getAttribute("role") === "search") {
          const q = form.querySelector("input[type=search]");
          if (q && q.value !== q.defaultValue) return true;
          continue;
        }
        if ((form.dataset.uploads || "0") !== "0" || form.hasAttribute("data-unsaved")) return true;
        const fields = form.querySelectorAll("input[type=text], textarea");
        if (!fields.length) continue;
        if (active && (form.contains(active) || active.form === form)) return true;
        if ([...fields].some((f) => f.value !== f.defaultValue)) return true;
      }
      return false;
    }

    // A seq reads "<boot>.<n>"; see store.Seq.
    function parseSeq(seq) {
      const dot = seq ? seq.lastIndexOf(".") : -1;
      return dot < 0 ? null : { boot: seq.slice(0, dot), n: Number(seq.slice(dot + 1)) };
    }
    const pageSeq = () => document.getElementById("page")?.dataset.seq || "";

    // A page from another boot is behind; within one it is behind while its
    // n is below the latest heard. A #page without a seq counts as behind,
    // since a refresh brings one.
    function behind() {
      if (force) return true;
      const known = parseSeq(latest);
      if (!known || !document.getElementById("page")) return false;
      const page = parseSeq(pageSeq());
      return !page || page.boot !== known.boot || page.n < known.n;
    }

    // Within a boot a seq only moves forward: changes queued before a
    // stream's opening seq arrive after it, carrying lower ones.
    function hear(seq) {
      const heard = parseSeq(seq);
      const known = parseSeq(latest);
      if (!heard || (known && heard.boot === known.boot && heard.n <= known.n)) return;
      latest = seq;
      arrive();
    }

    // A page rendered after the latest seq heard is newer news than that
    // seq. A restarted server's pages carry its new boot before its stream
    // reconnects to say so, and until then every page it served would look
    // behind and refresh again as soon as it landed.
    function adopt() {
      const page = parseSeq(pageSeq());
      const known = parseSeq(latest);
      if (page && (!known || page.boot !== known.boot || page.n > known.n)) latest = pageSeq();
    }

    function attempt() {
      if (running || debounce || later || document.hidden) return;
      if (!behind()) {
        say("");
        return;
      }
      if (document.querySelector(".htmx-request, .htmx-swapping, .htmx-settling")) return;
      if (held()) {
        say("Updates waiting");
        return;
      }
      // Whatever the state above says, live refreshes stay two seconds apart,
      // the pace a steady trickle of changes gets anyway: a mistake in that
      // state must cost a request every two seconds, not a request loop
      // against the server.
      const now = Date.now();
      if (now - lastRefresh < 2000) {
        wait(lastRefresh + 2000 - now);
        return;
      }
      lastRefresh = now;
      refresh();
    }
    const poke = () => setTimeout(attempt);
    function wait(ms) {
      later = setTimeout(() => {
        later = 0;
        attempt();
      }, ms);
    }

    // A burst, an agent filing ten items say, becomes one refresh, and a
    // steady trickle still refreshes every two seconds.
    function arrive() {
      clearTimeout(debounce);
      burst = burst || Date.now();
      debounce = setTimeout(
        () => {
          debounce = 0;
          burst = 0;
          attempt();
        },
        Math.max(0, Math.min(300, burst + 2000 - Date.now())),
      );
    }

    // A refresh that fails is tried again, backing off, for as long as the
    // page stays behind: a server mid-deploy answers again soon enough, and
    // the change it was for may be the only one for a while.
    function refresh() {
      const forced = force;
      force = false;
      running = true;
      overtaken = false;
      failed = false;
      let swapped = false;
      say("");
      htmx
        .ajax("GET", location.pathname + location.search, {
          target: "#page",
          select: "#page",
          swap: "outerHTML",
          headers: { [liveHeader]: "1" },
        })
        .then(
          () => {
            swapped = settle();
          },
          () => {
            failed = true;
          },
        )
        .finally(() => {
          running = false;
          if (swapped) adopt();
          else force = force || forced;
          if (failed) {
            wait(failDelay);
            failDelay = Math.min(failDelay * 2, 30000);
            return;
          }
          failDelay = 1000;
          poke();
        });
    }

    document.addEventListener("htmx:beforeRequest", (e) => {
      if (running && !isLive(e.detail.requestConfig)) overtaken = true;
    });

    document.addEventListener("htmx:beforeSwap", (e) => {
      const d = e.detail;
      if (!isLive(d.requestConfig)) return;
      if (d.isError || !d.shouldSwap) {
        failed = true;
        return;
      }
      if (overtaken || held()) {
        d.shouldSwap = false;
        return;
      }
      const doc = new DOMParser().parseFromString(d.serverResponse, "text/html");
      const page = doc.getElementById("page");
      if (!page || page.dataset.asset !== asset) {
        e.preventDefault();
        location.reload();
        return;
      }
      // Only #page goes in: a toast in the answer was shown when its action
      // happened, and autofocus would pull focus into a form no one is using.
      for (const el of page.querySelectorAll("[autofocus]")) el.removeAttribute("autofocus");
      const title = doc.querySelector("title");
      d.serverResponse = (title ? title.outerHTML : "") + page.outerHTML;
      before = snapshot();
    });

    function snapshot() {
      const drawer = document.querySelector("#page .drawer .body");
      return {
        revs: new Set([...document.querySelectorAll("#page [data-rev]")].map((el) => el.dataset.rev)),
        scroll: drawer ? drawer.scrollTop : 0,
        focus: focused(),
      };
    }

    // Names the focused element in a way that survives the swap replacing
    // it: its id, or for links, which have none, a selector and its place
    // among the matches. Links share hrefs (the wordmark, the Open tab and
    // the drawer's Close all go to /), and the click-away backdrop, which no
    // keyboard reaches, is left out. The search field keeps itself (typing).
    function focused() {
      const el = document.activeElement;
      if (!el || !el.closest("#page")) return null;
      let selector = "";
      if (el.id) selector = "#" + CSS.escape(el.id);
      else if (el.hasAttribute("href"))
        selector = `#page a[href="${CSS.escape(el.getAttribute("href"))}"]:not([tabindex="-1"], [aria-hidden="true"])`;
      if (!selector) return null;
      return { selector, index: [...document.querySelectorAll(selector)].indexOf(el) };
    }

    // What the swap took is put back: the drawer's scroll and the keyboard's
    // place, which would otherwise fall back to the top of the document.
    // Rows the page did not show a moment ago light up; comparing with the
    // page just before this swap, not with the last live one, keeps a change
    // made in this tab, which its own swap already showed, from lighting up
    // again.
    function settle() {
      const was = before;
      before = null;
      if (!was) return false;
      for (const el of document.querySelectorAll("#page [data-rev]")) {
        if (!was.revs.has(el.dataset.rev)) el.classList.add("fresh");
      }
      const drawer = document.querySelector("#page .drawer .body");
      if (drawer) drawer.scrollTop = was.scroll;
      const el = was.focus && document.querySelectorAll(was.focus.selector)[was.focus.index];
      if (el) el.focus({ preventScroll: true });
      return true;
    }

    document.addEventListener("htmx:afterRequest", poke);
    document.addEventListener("htmx:afterSettle", () => {
      adopt();
      poke();
    });
    document.addEventListener("focusout", poke);
    // typing a search back to what it was releases the hold without a blur
    document.addEventListener("input", poke);
    // toggle does not bubble, and closing a popover moves no focus
    document.addEventListener("toggle", poke, true);

    // Every stream opens with the current seq, so a page that missed changes
    // while it had none, hidden or between its render and the stream, finds
    // out and refreshes, and one that missed nothing does not. A stream the
    // server refused, with 204 when signed out or a proxy's error during a
    // deploy, is closed for good, and EventSource cannot say which; a
    // refresh finds out, and htmx takes a signed-out page to the login page
    // whole. A stream that opens again settles it first: the session holds.
    let source = null;
    let backoff = 1000;
    let retry = 0;
    function connect() {
      retry = 0;
      if (source || document.hidden) return;
      const s = new EventSource("/events");
      source = s;
      s.onopen = () => {
        backoff = 1000;
        force = false;
      };
      s.addEventListener("seq", (e) => hear(e.data));
      s.onmessage = (e) => hear(JSON.parse(e.data).seq);
      s.onerror = () => {
        if (s.readyState !== EventSource.CLOSED) return;
        source = null;
        force = true;
        arrive();
        retry = setTimeout(connect, backoff);
        backoff = Math.min(backoff * 2, 60000);
      };
    }
    connect();

    // Over HTTP/1.1 a browser opens at most six connections to a host and
    // each stream holds one, so a hidden tab lets its stream go rather than
    // starve the tab in front. Coming back reconnects.
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) {
        clearTimeout(retry);
        if (source) source.close();
        source = null;
      } else {
        connect();
      }
      poke();
    });
  }
})();
