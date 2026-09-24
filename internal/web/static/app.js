// Images pasted, dropped or picked into a body upload to /image and land in
// the text as the marker the server draws. Nothing else on the page needs this.
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

function insertLine(ta, text) {
  const { selectionStart: start, selectionEnd: end, value } = ta;
  const before = start > 0 && value[start - 1] !== "\n" ? "\n" : "";
  const after = value[end] === "\n" ? "" : "\n";
  ta.setRangeText(before + text + after, start, end, "end");
  if (!after) ta.selectionStart = ta.selectionEnd = ta.selectionEnd + 1;
}

function replaceText(ta, from, to) {
  const i = ta.value.indexOf(from);
  if (i < 0) return;
  let end = i + from.length;
  if (to === "" && ta.value[end] === "\n") end++;
  ta.setRangeText(to, i, end, "preserve");
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
    insertLine(ta, placeholder);
    if (form) setBusy(form, 1);
    post(file)
      .then((markdown) => replaceText(ta, placeholder, markdown))
      .catch((err) => {
        replaceText(ta, placeholder, "");
        status.textContent = err.message;
      })
      .finally(() => {
        if (form) setBusy(form, -1);
      });
  }
}

for (const ta of document.querySelectorAll("textarea[name=body]")) {
  const attach = document.querySelector(`.attach[data-for="${ta.id}"]`);
  if (!attach) continue;
  const status = attach.querySelector(".upload-status");
  const picker = attach.querySelector("input[type=file]");
  attach.hidden = false;

  attach.querySelector("button").addEventListener("click", () => picker.click());
  picker.addEventListener("change", () => {
    upload(ta, status, images(picker.files));
    picker.value = "";
  });

  ta.addEventListener("paste", (e) => {
    const files = images(e.clipboardData && e.clipboardData.files);
    if (!files.length) return;
    e.preventDefault();
    upload(ta, status, files);
  });

  const dragsFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");
  ta.addEventListener("dragover", (e) => {
    if (!dragsFiles(e)) return;
    e.preventDefault();
    ta.classList.add("drop-target");
  });
  ta.addEventListener("dragleave", () => ta.classList.remove("drop-target"));
  ta.addEventListener("drop", (e) => {
    ta.classList.remove("drop-target");
    const files = images(e.dataTransfer && e.dataTransfer.files);
    if (!files.length) return;
    e.preventDefault();
    ta.focus();
    upload(ta, status, files);
  });

  if (ta.form) {
    ta.form.addEventListener("submit", (e) => {
      if ((ta.form.dataset.uploads || "0") !== "0") e.preventDefault();
    });
  }
}
