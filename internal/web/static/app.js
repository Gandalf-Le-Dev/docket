// Images pasted, dropped or picked into a body upload to /image and land in
// the text as the marker the server draws; and a swap that leaves what it
// brought out of sight scrolls to it. Bodies arrive with htmx swaps as well as
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

  // Swaps from the list keep its scroll, which can leave what they brought out
  // of sight: a failed action's message at the top of the list, or the drawer,
  // which a narrow screen stacks above the list.
  document.addEventListener("htmx:afterSettle", () => {
    const shown = document.querySelector("#page .error") || document.querySelector(".drawer");
    if (!shown) return;
    const { top } = shown.getBoundingClientRect();
    if (top < 0 || top > window.innerHeight) shown.scrollIntoView();
  });

  // A boosted link can lead off the page this script knows, to /healthz say;
  // with no #page in the answer the swap would blank the screen, so the
  // browser loads it as the page it is.
  document.addEventListener("htmx:beforeSwap", (e) => {
    const { boosted, requestConfig, xhr, serverResponse, isError } = e.detail;
    if (!boosted || requestConfig.verb !== "get" || isError || / id="page"/.test(serverResponse)) return;
    e.preventDefault();
    location.href = xhr.responseURL || requestConfig.path;
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
})();
