// A secret's scopes as tags. The page serves each scopes field as a
// select of every scope the loaded jobs offer, with the secret's own
// picked, which works as it is without this script. Here the select is
// hidden and stays what the form sends: each picked scope is shown as a
// tag with a button that unpicks it, and a second select below lists the
// scopes not picked yet, so a scope is only ever one that was offered.
(() => {
  for (const field of document.querySelectorAll("select[data-tags]")) {
    const tags = document.createElement("ul");
    tags.className = "scope-tags";
    const add = document.createElement("select");
    add.className = "entry";
    add.setAttribute("aria-label", "Add a scope");
    field.hidden = true;
    field.after(tags, add);

    const render = () => {
      tags.replaceChildren();
      add.replaceChildren(new Option("Add a scope...", ""));
      for (const group of field.querySelectorAll("optgroup")) {
        const offered = document.createElement("optgroup");
        offered.label = group.label;
        for (const option of group.querySelectorAll("option")) {
          if (!option.selected) {
            offered.append(new Option(option.value, option.value));
            continue;
          }
          const tag = document.createElement("li");
          tag.className = "tag scope-tag";
          const name = document.createElement("span");
          name.textContent = option.value;
          const remove = document.createElement("button");
          remove.type = "button";
          remove.textContent = "x";
          remove.setAttribute("aria-label", "Remove " + option.value);
          // The tag goes before the click reaches the page, and nav.js would
          // read a click on what is no longer in the form as one outside it
          // and close the form.
          remove.addEventListener("click", (event) => {
            event.stopPropagation();
            option.selected = false;
            render();
            add.focus();
          });
          tag.append(name, remove);
          tags.append(tag);
        }
        if (offered.children.length > 0) add.append(offered);
      }
      add.disabled = add.options.length === 1;
    };

    add.addEventListener("change", () => {
      const option = [...field.options].find((o) => o.value === add.value);
      if (option) option.selected = true;
      render();
    });
    render();
  }
})();
