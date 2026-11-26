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
