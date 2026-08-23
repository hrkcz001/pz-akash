// The dashboard's client-side behaviour, in full.
//
// v1's equivalent was about 400 lines inside the page, and most of it was
// localisation: the server always emitted Russian, and clicking EN walked the DOM
// replacing sixty strings by element id. Locales are a server render now, so what
// is left is the three things a browser genuinely has to do — keep the header
// current, open the unlock prompt, and stream a file up.
//
// No string in here is a message. Everything a person reads comes from data-msg-*
// attributes the template wrote, which means the catalog stays the only place
// wording lives and nothing is interpolated into a script.
(function () {
  "use strict";

  var body = document.body;
  var msg = body.dataset;

  // --- Unlock prompt -------------------------------------------------------

  var modal = document.getElementById("auth-modal");
  if (modal) {
    var realm = document.getElementById("modal-realm");
    var pwd = document.getElementById("modal-password");
    var err = document.getElementById("modal-error");
    var submit = document.getElementById("modal-submit");
    var form = document.getElementById("unlock-form");

    var showError = function (text) {
      err.textContent = text;
      err.classList.add("shown");
      // The shake has to be re-triggered, not just re-applied: the animation
      // only runs on the transition into the class.
      modal.querySelector(".modal-box").classList.remove("shake");
      void modal.offsetWidth;
      modal.querySelector(".modal-box").classList.add("shake");
    };

    var open = function (which) {
      if (which) realm.value = which;
      modal.classList.add("open");
      pwd.focus();
    };

    var close = function () {
      modal.classList.remove("open");
      pwd.value = "";
      err.classList.remove("shown");
    };

    document.querySelectorAll("[data-unlock]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        open(btn.dataset.unlock);
      });
    });

    document.getElementById("modal-cancel").addEventListener("click", close);

    modal.addEventListener("click", function (e) {
      if (e.target === modal) close();
    });

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && modal.classList.contains("open")) close();
    });

    form.addEventListener("submit", function (e) {
      if (!pwd.value.trim()) {
        e.preventDefault();
        showError(msg.msgEmpty);
        return;
      }
      // The form posts for real; this is only so a slow verify does not look
      // like a dead button. The page that comes back is rendered either way.
      submit.disabled = true;
      submit.textContent = msg.msgVerifying;
    });

    // A failed attempt comes back as a page with the prompt already open.
    if (modal.classList.contains("open")) pwd.focus();
  }

  // --- Copy buttons --------------------------------------------------------

  // A shared Akash endpoint gives players an address nobody can retype from
  // memory: the provider's own hostname, and a five-digit port it drew from its
  // pool rather than the one the SDL asked for. So both get a button.

  // The clipboard, twice, because the modern API is missing exactly where this
  // page usually runs. navigator.clipboard needs a secure context, and a
  // controller on a shared endpoint is reached over plain http at a provider
  // hostname — so in production the first branch does not exist and the textarea
  // is the one doing the work. It is still tried first: execCommand is the half
  // browsers have been threatening to remove for years.
  var toClipboard = function (text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    var ta = document.createElement("textarea");
    ta.value = text;
    // Off-screen rather than hidden: a display:none element cannot hold a
    // selection. readOnly stops a phone opening its keyboard over the page.
    ta.readOnly = true;
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, text.length);
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok ? Promise.resolve() : Promise.reject();
  };

  document.querySelectorAll(".addr-copy").forEach(function (btn) {
    // The idle wording is whatever the server rendered, so restoring it needs no
    // second catalog lookup and no string here.
    var idle = btn.getAttribute("title");
    var timer = 0;
    btn.addEventListener("click", function () {
      // The value comes from the DOM beside the button rather than from an
      // attribute on it. One address, one place: a data-copy="..." copy is a
      // second rendering of the same thing, free to drift from the visible one.
      var value = btn.parentNode.querySelector(".address-text");
      if (!value) return;
      toClipboard(value.textContent.trim()).then(function () {
        btn.classList.add("copied");
        btn.setAttribute("title", msg.msgCopied);
        btn.setAttribute("aria-label", msg.msgCopied);
        clearTimeout(timer);
        // Long enough to read, short enough that copying the port right after
        // the hostname still gets a confirmation of its own.
        timer = setTimeout(function () {
          btn.classList.remove("copied");
          btn.setAttribute("title", idle);
          btn.setAttribute("aria-label", idle);
        }, 1400);
      }, function () {
        // A refused clipboard gets no dialog. The value is on screen and
        // selectable, which is what a person falls back to anyway.
      });
    });
  });

  // --- Header poll ---------------------------------------------------------

  var stageBox = document.getElementById("address-widget-container");
  var pollMs = parseInt(body.dataset.pollMs || "0", 10);

  if (stageBox && pollMs > 0) {
    var apply = function (s) {
      // A stage change rearranges the card — an address grid where a banner was —
      // so the server renders it rather than the browser assembling it.
      if (s.stage !== stageBox.dataset.stage) {
        window.location.reload();
        return;
      }

      var badge = document.getElementById("status-badge");
      badge.className = "status-badge " + s.badge.class;
      badge.querySelector(".status-dot").className = "status-dot " + s.badge.dot;
      document.getElementById("status-text").textContent = s.badge.text;

      document.getElementById("players-text").textContent = s.players.text;
      var stale = document.getElementById("players-stale");
      stale.textContent = "· " + s.players.stale_text;
      stale.hidden = !s.players.stale;
      // Hidden off StageOnline, the same rule the server rendered with. Without
      // this the badge a page loaded offline would stay hidden after the server
      // came up — the poll swaps the stage without a navigation.
      document.getElementById("players-badge").hidden = !s.show_players;

      var price = document.getElementById("price-badge");
      document.getElementById("price-text").textContent = s.price;
      price.hidden = !s.show_price;
    };

    var tick = function () {
      // Not while someone is typing a password into the prompt.
      if (modal && modal.classList.contains("open")) return;

      fetch("/api/status?lang=" + encodeURIComponent(document.documentElement.lang), {
        credentials: "same-origin",
        headers: { Accept: "application/json" }
      })
        .then(function (r) {
          if (!r.ok) throw new Error(String(r.status));
          return r.json();
        })
        .then(apply)
        // A failed poll leaves the last render in place. It is a reading that
        // did not arrive, not a server that went offline, and claiming the
        // second would be the same mistake v1 made with the player count.
        .catch(function () {});
    };

    setInterval(tick, pollMs);
  }

  // --- Archive upload ------------------------------------------------------

  var upload = document.getElementById("upload-form");
  if (upload) {
    var file = document.getElementById("upload-file");
    var btn = document.getElementById("upload-submit");
    var status = document.getElementById("upload-status");

    upload.addEventListener("submit", function (e) {
      e.preventDefault();
      var f = file.files[0];
      if (!f) return;

      status.className = "upload-status";
      status.textContent = msg.msgUploading;
      btn.disabled = true;
      btn.textContent = msg.msgUploading;

      // PUT to the same path the archive is served from, which is also the
      // endpoint the agent uses. v1 had a second, multipart route for this, so
      // there were two ways in with two sets of checks.
      fetch("/backups/" + encodeURIComponent(f.name), {
        method: "PUT",
        credentials: "same-origin",
        body: f
      })
        .then(function (r) {
          if (!r.ok) throw new Error(String(r.status));
          status.className = "upload-status done";
          status.textContent = msg.msgUploaded;
          window.location.reload();
        })
        .catch(function () {
          status.className = "upload-status failed";
          status.textContent = msg.msgUploadFailed;
          btn.disabled = false;
          btn.textContent = msg.msgUpload;
        });
    });
  }
})();
