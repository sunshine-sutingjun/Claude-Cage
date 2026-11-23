import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { MacOSAppWindow } from "../src/macos-claude-window";
import { useDesktopFiles } from "../src/browser-downloads";
import "../src/style.css";

function Fixture() {
  const [notice, setNotice] = useState("");
  const [windowStatus, setWindowStatus] = useState("");
  const files = useDesktopFiles(true, setNotice, true);
  return <div className="app">
    <MacOSAppWindow visible uploads={files.uploads} onStatus={setNotice} onWindowStatus={setWindowStatus} onConfirm={async () => true} />
    <footer className="status-bar"><output>{windowStatus || notice}</output></footer>
  </div>;
}
createRoot(document.getElementById("root")).render(<Fixture />);
