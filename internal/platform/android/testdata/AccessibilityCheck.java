package dev.oneo.nsgo;

import java.io.DataInputStream;
import java.io.IOException;
import java.util.Objects;

/**
 * Verifies Accessibility.describe on the JVM using the exact spoken word
 * arrays encoded by Go's writeDirection for English and Simplified Chinese.
 */
public final class AccessibilityCheck {
    public static void main(String[] args) throws IOException {
        DataInputStream in = new DataInputStream(System.in);
        String[] en = readDirectionWords(in);
        String[] zh = readDirectionWords(in);

        // 1. All 11 NSH 0.4.0 roles in English and Simplified Chinese.
        checkRole(Protocol.ROLE_LISTBOX, false, null, "list box", false, false, en);
        checkRole(Protocol.ROLE_LISTBOX, false, null, "列表框", false, false, zh);

        checkRole(Protocol.ROLE_OPTION, false, null, "option", false, false, en);
        checkRole(Protocol.ROLE_OPTION, false, null, "选项", false, false, zh);

        // Combobox: view trigger vs editable InputField.
        checkRole(Protocol.ROLE_COMBOBOX, false, null, "combo box", false, false, en);
        checkRole(Protocol.ROLE_COMBOBOX, false, null, "组合框", false, false, zh);
        checkRole(Protocol.ROLE_COMBOBOX, true, "android.widget.EditText", "combo box", false, false, en);
        checkRole(Protocol.ROLE_COMBOBOX, true, "android.widget.EditText", "组合框", false, false, zh);

        checkRole(Protocol.ROLE_GROUP, false, null, "group", false, false, en);
        checkRole(Protocol.ROLE_GROUP, false, null, "分组", false, false, zh);

        checkRole(Protocol.ROLE_REGION, false, null, "region", false, false, en);
        checkRole(Protocol.ROLE_REGION, false, null, "区域", false, false, zh);

        checkRole(Protocol.ROLE_NAVIGATION, false, null, "navigation", false, false, en);
        checkRole(Protocol.ROLE_NAVIGATION, false, null, "导航", false, false, zh);

        checkRole(Protocol.ROLE_TREE, false, null, "tree", false, false, en);
        checkRole(Protocol.ROLE_TREE, false, null, "树形视图", false, false, zh);

        checkRole(Protocol.ROLE_TREE_ITEM, false, null, "tree item", false, false, en);
        checkRole(Protocol.ROLE_TREE_ITEM, false, null, "树形项", false, false, zh);

        checkRole(Protocol.ROLE_TOOLBAR, false, null, "toolbar", false, false, en);
        checkRole(Protocol.ROLE_TOOLBAR, false, null, "工具栏", false, false, zh);

        // Checkable menu items keep CheckBox/RadioButton classes and localized "menu item" role descriptions.
        checkRole(Protocol.ROLE_MENU_ITEM_CHECKBOX, false, "android.widget.CheckBox", "menu item", false, true, en);
        checkRole(Protocol.ROLE_MENU_ITEM_CHECKBOX, false, "android.widget.CheckBox", "菜单项", false, true, zh);
        checkRole(Protocol.ROLE_MENU_ITEM_RADIO, false, "android.widget.RadioButton", "menu item", false, true, en);
        checkRole(Protocol.ROLE_MENU_ITEM_RADIO, false, "android.widget.RadioButton", "菜单项", false, true, zh);

        // 2. Checked, unchecked, and mixed states on menuitemcheckbox and menuitemradio.
        Accessibility.Description cbChecked = Accessibility.describe(
                Protocol.ROLE_MENU_ITEM_CHECKBOX, false, true, false, false, false, false, false, "", en);
        assertEq("cbChecked.checkable", true, cbChecked.checkable);
        assertEq("cbChecked.checked", true, cbChecked.checked);
        assertEq("cbChecked.state", null, cbChecked.stateDescription);

        Accessibility.Description cbMixedEn = Accessibility.describe(
                Protocol.ROLE_MENU_ITEM_CHECKBOX, false, true, true, false, false, false, false, "", en);
        assertEq("cbMixedEn.checked", false, cbMixedEn.checked);
        assertEq("cbMixedEn.state", "partially checked", cbMixedEn.stateDescription);

        Accessibility.Description cbMixedZh = Accessibility.describe(
                Protocol.ROLE_MENU_ITEM_CHECKBOX, false, true, true, false, false, false, false, "", zh);
        assertEq("cbMixedZh.state", "部分选中", cbMixedZh.stateDescription);

        Accessibility.Description radioChecked = Accessibility.describe(
                Protocol.ROLE_MENU_ITEM_RADIO, false, true, false, false, false, false, false, "", en);
        assertEq("radioChecked.checkable", true, radioChecked.checkable);
        assertEq("radioChecked.checked", true, radioChecked.checked);

        Accessibility.Description radioUnchecked = Accessibility.describe(
                Protocol.ROLE_MENU_ITEM_RADIO, false, false, false, false, false, false, false, "", en);
        assertEq("radioUnchecked.checkable", true, radioUnchecked.checkable);
        assertEq("radioUnchecked.checked", false, radioUnchecked.checked);

        // 3. Pressed and Busy states individually and combined.
        Accessibility.Description pressedEn = Accessibility.describe(
                Protocol.ROLE_BUTTON, false, false, false, true, false, false, false, "", en);
        assertEq("pressedEn.className", "android.widget.Button", pressedEn.className);
        assertEq("pressedEn.state", "pressed", pressedEn.stateDescription);

        Accessibility.Description pressedZh = Accessibility.describe(
                Protocol.ROLE_BUTTON, false, false, false, true, false, false, false, "", zh);
        assertEq("pressedZh.state", "已按下", pressedZh.stateDescription);

        Accessibility.Description notPressed = Accessibility.describe(
                Protocol.ROLE_BUTTON, false, false, false, false, false, false, false, "", en);
        assertEq("notPressed.state", null, notPressed.stateDescription);

        Accessibility.Description busyEn = Accessibility.describe(
                Protocol.ROLE_REGION, false, false, false, false, true, false, false, "", en);
        assertEq("busyEn.role", "region", busyEn.roleDescription);
        assertEq("busyEn.state", "busy", busyEn.stateDescription);

        Accessibility.Description busyZh = Accessibility.describe(
                Protocol.ROLE_REGION, false, false, false, false, true, false, false, "", zh);
        assertEq("busyZh.role", "区域", busyZh.roleDescription);
        assertEq("busyZh.state", "忙碌", busyZh.stateDescription);

        Accessibility.Description combinedEn = Accessibility.describe(
                Protocol.ROLE_COMBOBOX, false, false, false, true, true, true, true, "Apple", en);
        assertEq("combinedEn.role", "combo box", combinedEn.roleDescription);
        assertEq("combinedEn.state", "pressed, expanded, busy, Apple", combinedEn.stateDescription);

        Accessibility.Description combinedZh = Accessibility.describe(
                Protocol.ROLE_COMBOBOX, false, false, false, true, true, true, false, "苹果", zh);
        assertEq("combinedZh.role", "组合框", combinedZh.roleDescription);
        assertEq("combinedZh.state", "已按下，已收起，忙碌，苹果", combinedZh.stateDescription);

        // 4. Empty spoken array (before ItemDirection arrives) stays null-safe.
        Accessibility.Description beforeDirection = Accessibility.describe(
                Protocol.ROLE_TREE_ITEM, false, false, false, true, true, true, true, "", new String[0]);
        assertEq("beforeDirection.role", null, beforeDirection.roleDescription);
        assertEq("beforeDirection.state", null, beforeDirection.stateDescription);
    }

    private static String[] readDirectionWords(DataInputStream in) throws IOException {
        in.readUnsignedByte(); // RTL flag
        int n = in.readInt();
        String[] words = new String[n];
        for (int i = 0; i < n; i++) {
            words[i] = Protocol.readString(in);
        }
        return words;
    }

    private static void checkRole(int role, boolean isField, String wantClass, String wantRole,
            boolean wantHeading, boolean wantCheckable, String[] spoken) {
        Accessibility.Description d = Accessibility.describe(
                role, isField, false, false, false, false, false, false, "", spoken);
        assertEq("role " + role + " className", wantClass, d.className);
        assertEq("role " + role + " roleDescription", wantRole, d.roleDescription);
        assertEq("role " + role + " heading", wantHeading, d.heading);
        assertEq("role " + role + " checkable", wantCheckable, d.checkable);
    }

    private static void assertEq(String label, Object want, Object got) {
        if (!Objects.equals(want, got)) {
            throw new AssertionError(label + ": got " + got + ", want " + want);
        }
    }
}
