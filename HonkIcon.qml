import QtQuick

// Theme-colored native mark, drawn in QML rather than from icon.svg so it
// follows the bar foreground (same approach as omarchy.tailscale's icon).
// A ring with a center dot; the dot is hollow when honk is not healthy and a
// badge dot appears on error.
Item {
  id: root

  property real iconSize: 16
  property color color: "white"
  property color badgeColor: "red"
  property bool healthy: false
  property bool warning: false

  implicitWidth: iconSize
  implicitHeight: iconSize
  width: iconSize
  height: iconSize

  Rectangle {
    anchors.fill: parent
    radius: width / 2
    color: "transparent"
    border.width: Math.max(1.5, root.iconSize / 9)
    border.color: root.color
  }

  Rectangle {
    width: root.iconSize * 0.36
    height: width
    radius: width / 2
    anchors.centerIn: parent
    color: root.healthy ? root.color : "transparent"
    border.width: root.healthy ? 0 : Math.max(1, root.iconSize / 12)
    border.color: root.color
  }

  Rectangle {
    visible: root.warning
    width: root.iconSize * 0.34
    height: width
    radius: width / 2
    x: parent.width - width * 0.8
    y: -height * 0.2
    color: root.badgeColor
  }
}
