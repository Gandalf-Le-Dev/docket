// Images pasted, dropped or picked into a body upload to /image and land in
// the text as the marker the server draws; a swap that leaves what it
// brought out of sight scrolls to it; and a page refreshes itself when an
// item changes anywhere (see live). Bodies arrive with htmx swaps as well as
// with the page, so every listener sits on the document and finds its field
// when the event comes, rather than binding to fields once at load.
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
      if (form) setBusy(form, 1);
      post(file)
        .then((markdown) => replaceText(ta, placeholder, markdown))
        .catch((err) => {
          // the line breaks are left if the text around the placeholder was edited meanwhile
          replaceText(ta, inserted, "") || replaceText(ta, placeholder, "");
          status.textContent = err.message;
        })
        .finally(() => {
          if (form) setBusy(form, -1);
        });
    }
  }

  function bodyOf(target) {
    const ta = target instanceof Element ? target.closest("textarea[name=body]") : null;
    const attach = ta && ta.id ? document.querySelector(`.attach[data-for="${ta.id}"]`) : null;
    return attach ? { ta, attach, status: attach.querySelector(".upload-status") } : null;
  }

  function reveal(root) {
    for (const attach of root.querySelectorAll(".attach[hidden]")) attach.hidden = false;
  }
  reveal(document);
  document.addEventListener("htmx:load", (e) => reveal(e.target));

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

  const liveHeader = "Docket-Live";
  const isLive = (config) => Boolean(config && config.headers && config.headers[liveHeader]);

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
    if (!boosted) return;
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
    // back with the item's new values.
    function held() {
      const active = document.activeElement;
      if (popoverOpen()) return true;
      for (const form of document.querySelectorAll("#page form")) {
        if (form.getAttribute("role") === "search") {
          const q = form.querySelector("input[type=search]");
          if (q && q === active && q.value !== q.defaultValue) return true;
          continue;
        }
        if ((form.dataset.uploads || "0") !== "0") return true;
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
      if (running || debounce || document.hidden) return;
      if (!behind()) {
        say("");
        return;
      }
      if (document.querySelector(".htmx-request, .htmx-swapping, .htmx-settling")) return;
      if (held()) {
        say("Updates waiting");
        return;
      }
      refresh();
    }
    const poke = () => setTimeout(attempt);

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

    // A refresh that fails is not tried again at once, which would spin
    // against a server that is down; the next change or reconnect brings
    // another.
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
          if (!failed) poke();
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
      // Only #page goes in: the toast a URL like ?did=closed carries was
      // shown when it happened, and autofocus would pull focus into a form
      // no one is using.
      for (const el of page.querySelectorAll("[autofocus]")) el.removeAttribute("autofocus");
      const title = doc.querySelector("title");
      d.serverResponse = (title ? title.outerHTML : "") + page.outerHTML;
      before = snapshot();
    });

    function snapshot() {
      const active = document.activeElement;
      const drawer = document.querySelector("#page .drawer .body");
      return {
        revs: new Set([...document.querySelectorAll("#page [data-rev]")].map((el) => el.dataset.rev)),
        scroll: drawer ? drawer.scrollTop : 0,
        search: Boolean(active && active.matches("#page input[type=search]")),
      };
    }

    // What the swap took is put back: the drawer's scroll, the search field's
    // focus. Rows the page did not show a moment ago light up; comparing with
    // the page just before this swap, not with the last live one, keeps a
    // change made in this tab, which its own swap already showed, from
    // lighting up again.
    function settle() {
      const was = before;
      before = null;
      if (!was) return false;
      for (const el of document.querySelectorAll("#page [data-rev]")) {
        if (!was.revs.has(el.dataset.rev)) el.classList.add("fresh");
      }
      const drawer = document.querySelector("#page .drawer .body");
      if (drawer) drawer.scrollTop = was.scroll;
      const q = was.search && document.querySelector("#page input[type=search]");
      if (q) {
        q.focus({ preventScroll: true });
        q.setSelectionRange(q.value.length, q.value.length);
      }
      return true;
    }

    document.addEventListener("htmx:afterRequest", poke);
    document.addEventListener("htmx:afterSettle", () => {
      adopt();
      poke();
    });
    document.addEventListener("focusout", poke);
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
