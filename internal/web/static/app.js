// Progressive enhancement. Every page works without this file.
(() => {
  const editor = document.querySelector("[data-editor]");
  if (editor) {
    const text = editor.querySelector("textarea");
    const initial = text.value;
    let submitting = false;

    // Cursor at the end, not the start.
    text.setSelectionRange(text.value.length, text.value.length);

    // Ctrl/Cmd+Enter: save and next (or "Save anyway" when shown).
    editor.querySelector("[data-hint]").hidden = false;
    text.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        editor.querySelector("button.primary").click();
      }
    });

    const copy = editor.querySelector("[data-copy-source]");
    copy.hidden = false;
    copy.addEventListener("click", () => {
      text.value = document.getElementById("source").textContent;
      text.focus();
    });

    editor.addEventListener("submit", () => { submitting = true; });
    window.addEventListener("beforeunload", (e) => {
      if (!submitting && text.value !== initial) e.preventDefault();
    });
  }

  // Click to select tokens and links meant for copying.
  for (const el of document.querySelectorAll("[data-select]")) {
    el.addEventListener("focus", () => el.select());
  }

  for (const el of document.querySelectorAll("[data-confirm]")) {
    el.addEventListener("click", (e) => {
      if (!confirm(el.dataset.confirm)) e.preventDefault();
    });
  }
})();
