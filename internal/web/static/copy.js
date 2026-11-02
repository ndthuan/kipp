// The Copy button beside a value shown once: a token or a password. It
// copies the value the button names and says so in the status beside it.
// Without this script, or without a clipboard the page may use (one served
// over plain HTTP to another machine has none), the button stays hidden
// and the value is selected whole at a click, to be copied by hand.
(() => {
  const clipboard = globalThis.navigator?.clipboard;
  if (!clipboard) return;
  for (const button of document.querySelectorAll("[data-copy]")) {
    const value = document.getElementById(button.getAttribute("data-copy"));
    const status = document.getElementById(button.getAttribute("data-copy-status"));
    if (!value) continue;
    const say = (text) => { if (status) status.textContent = text; };
    button.hidden = false;
    button.addEventListener("click", async () => {
      try {
        await clipboard.writeText(value.textContent);
        say("Copied.");
      } catch {
        say("The browser did not let the page copy it: select it and copy it by hand.");
      }
    });
  }
})();
